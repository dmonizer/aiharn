package shell

import (
	"bufio"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"aiharn/internal/execution"
)

// fakeStream implements Stream with configurable failure points.
type fakeStream struct {
	stdin      io.Writer
	reader     *bufio.Reader
	dead       bool
	resetCalls int
	closeCalls int
	signals    []string
}

func (f *fakeStream) Stdin() io.Writer          { return f.stdin }
func (f *fakeStream) Reader() *bufio.Reader     { return f.reader }
func (f *fakeStream) IsDead() bool              { return f.dead }
func (f *fakeStream) Reset()                    { f.resetCalls++ }
func (f *fakeStream) Close() error              { f.closeCalls++; return nil }
func (f *fakeStream) Signal(_ int, name string) { f.signals = append(f.signals, name) }

func testConfig() ExecConfig {
	return ExecConfig{
		Component: "test",
		ShellName: func() string { return "sh" },
		BeforeOutputErr: func(name string, err error) error {
			return errors.New("before-output: " + err.Error())
		},
		LogFinish: func(time.Time, int, execution.Result, error) {},
	}
}

type errorWriter struct{ err error }

func (w errorWriter) Write([]byte) (int, error) { return 0, w.err }

type errorReader struct{ err error }

func (r errorReader) Read([]byte) (int, error) { return 0, r.err }

type blockingWriter struct{ release <-chan struct{} }

func (w blockingWriter) Write(p []byte) (int, error) { <-w.release; return len(p), nil }

type blockingReader struct{ release <-chan struct{} }

func (r blockingReader) Read([]byte) (int, error) { <-r.release; return 0, io.EOF }

func TestExecIsDead(t *testing.T) {
	fs := &fakeStream{dead: true}
	_, err := Exec(context.Background(), "cmd", execution.ExecOptions{}, fs, testConfig())
	if !errors.Is(err, execution.ErrSessionReset) || !strings.Contains(err.Error(), "no longer usable") {
		t.Fatalf("err = %v, want session reset", err)
	}
}

func TestExecWriteError(t *testing.T) {
	fs := &fakeStream{stdin: errorWriter{errors.New("boom")}}
	_, err := Exec(context.Background(), "cmd", execution.ExecOptions{}, fs, testConfig())
	if !errors.Is(err, execution.ErrSessionReset) || !strings.Contains(err.Error(), "write: boom") {
		t.Fatalf("err = %v, want write error", err)
	}
	if fs.resetCalls != 1 || fs.closeCalls != 0 {
		t.Fatalf("reset/close calls = %d/%d, want 1/0", fs.resetCalls, fs.closeCalls)
	}
}

func TestExecCancelDuringWrite(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	fs := &fakeStream{stdin: blockingWriter{release}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		_, err := Exec(ctx, "cmd", execution.ExecOptions{}, fs, testConfig())
		done <- err
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, execution.ErrSessionReset) || !strings.Contains(err.Error(), "cancelled while writing") {
			t.Fatalf("err = %v, want cancelled-while-writing", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Exec did not return after cancel")
	}
}

func TestExecCancelDuringBegin(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	fs := &fakeStream{stdin: io.Discard, reader: bufio.NewReader(blockingReader{release})}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		_, err := Exec(ctx, "cmd", execution.ExecOptions{}, fs, testConfig())
		done <- err
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, execution.ErrSessionReset) || !strings.Contains(err.Error(), "cancelled before command started") {
			t.Fatalf("err = %v, want cancelled-before-started", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Exec did not return after cancel")
	}
}

func TestExecBeginErrorNonReset(t *testing.T) {
	fs := &fakeStream{stdin: io.Discard, reader: bufio.NewReader(errorReader{errors.New("boom")})}
	_, err := Exec(context.Background(), "cmd", execution.ExecOptions{}, fs, testConfig())
	if err == nil || !strings.Contains(err.Error(), "before-output") {
		t.Fatalf("err = %v, want before-output error", err)
	}
	if fs.closeCalls != 1 || fs.resetCalls != 0 {
		t.Fatalf("reset/close calls = %d/%d, want 0/1", fs.resetCalls, fs.closeCalls)
	}
}

func TestExecBeginErrorReset(t *testing.T) {
	fs := &fakeStream{stdin: io.Discard, reader: bufio.NewReader(strings.NewReader(""))}
	_, err := Exec(context.Background(), "cmd", execution.ExecOptions{}, fs, testConfig())
	if err == nil || !strings.Contains(err.Error(), "before-output") {
		t.Fatalf("err = %v, want before-output error", err)
	}
	if fs.resetCalls != 1 || fs.closeCalls != 0 {
		t.Fatalf("reset/close calls = %d/%d, want 1/0", fs.resetCalls, fs.closeCalls)
	}
}

func TestFinishBody(t *testing.T) {
	fs := &fakeStream{}
	if _, err := finishBody(fs, Frame{}, execution.ErrSessionReset); err == nil || fs.resetCalls != 1 {
		t.Fatalf("reset path: resetCalls = %d, err = %v", fs.resetCalls, err)
	}

	fs = &fakeStream{}
	if _, err := finishBody(fs, Frame{}, &StreamWriteError{Err: errors.New("sink")}); err == nil || fs.resetCalls != 0 || fs.closeCalls != 0 {
		t.Fatalf("stream-error path must not reset or close: reset=%d close=%d err=%v", fs.resetCalls, fs.closeCalls, err)
	}

	fs = &fakeStream{}
	if _, err := finishBody(fs, Frame{}, errors.New("boom")); err == nil || fs.closeCalls != 1 {
		t.Fatalf("generic-error path: closeCalls = %d, err = %v", fs.closeCalls, err)
	}

	res, err := finishBody(&fakeStream{}, Frame{Stdout: []byte("out"), ExitCode: 3}, nil)
	if err != nil || res.Stdout != "out" || res.ExitCode != 3 {
		t.Fatalf("success path = (%v, %v)", res, err)
	}
}
