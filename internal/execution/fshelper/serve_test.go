package fshelper_test

import (
	"errors"
	"io"
	"testing"
	"time"

	"aiharn/internal/execution"
	"aiharn/internal/execution/fshelper"
	"aiharn/internal/execution/fsproto"
)

const testRoot = "/srv/work"

// fakeBackend is a scriptable Backend. Unset operations return a zero value.
type fakeBackend struct {
	target string

	readFn    func(name string, maxBytes int64) (execution.FileSnapshot, error)
	createFn  func(name string, content []byte) (execution.FileStat, error)
	replaceFn func(name string, content []byte, expected execution.FileVersion) (execution.FileStat, error)
	closeErr  error

	readCalls    int
	createCalls  int
	replaceCalls int
	closeCalls   int
	lastExpected execution.FileVersion
}

func (b *fakeBackend) Target() string { return b.target }

func (b *fakeBackend) Read(name string, maxBytes int64) (execution.FileSnapshot, error) {
	b.readCalls++
	if b.readFn != nil {
		return b.readFn(name, maxBytes)
	}
	return execution.FileSnapshot{Stat: execution.FileStat{Name: name}}, nil
}

func (b *fakeBackend) Create(name string, content []byte) (execution.FileStat, error) {
	b.createCalls++
	if b.createFn != nil {
		return b.createFn(name, content)
	}
	return execution.FileStat{Name: name}, nil
}

func (b *fakeBackend) Replace(name string, content []byte, expected execution.FileVersion) (execution.FileStat, error) {
	b.replaceCalls++
	b.lastExpected = expected
	if b.replaceFn != nil {
		return b.replaceFn(name, content, expected)
	}
	return execution.FileStat{Name: name}, nil
}

func (b *fakeBackend) Close() error {
	b.closeCalls++
	return b.closeErr
}

// helperProcess drives Serve over an in-memory pipe pair.
type helperProcess struct {
	t          *testing.T
	toHelper   *io.PipeWriter
	fromHelper *io.PipeReader
	done       chan error
	backend    *fakeBackend
	openRoot   string
	openErr    error
}

type helperOption func(*helperProcess)

func withBackend(b *fakeBackend) helperOption {
	return func(h *helperProcess) { h.backend = b }
}

func withOpenError(err error) helperOption {
	return func(h *helperProcess) { h.openErr = err }
}

func startHelper(t *testing.T, opts ...helperOption) *helperProcess {
	t.Helper()
	h := &helperProcess{t: t, backend: &fakeBackend{target: "fake:" + testRoot}, done: make(chan error, 1)}
	for _, opt := range opts {
		opt(h)
	}
	reqR, reqW := io.Pipe()
	respR, respW := io.Pipe()
	h.toHelper = reqW
	h.fromHelper = respR
	go func() {
		h.done <- fshelper.Serve(reqR, respW, func(root string) (fshelper.Backend, error) {
			h.openRoot = root
			if h.openErr != nil {
				return nil, h.openErr
			}
			return h.backend, nil
		})
	}()
	t.Cleanup(func() {
		_ = reqW.Close()
		_ = respR.Close()
		select {
		case <-h.done:
		case <-time.After(5 * time.Second):
			t.Errorf("helper did not stop")
		}
	})
	return h
}

// initSession performs the init handshake with the given protocol version and
// returns the response.
func (h *helperProcess) initSession(protocol int, maxFileBytes int64) fsproto.Response {
	h.t.Helper()
	return h.request(fsproto.Request{
		Op:           fsproto.OpInit,
		Protocol:     protocol,
		Root:         testRoot,
		MaxFileBytes: maxFileBytes,
	})
}

func (h *helperProcess) request(req fsproto.Request) fsproto.Response {
	h.t.Helper()
	if err := fsproto.WriteFrame(h.toHelper, req); err != nil {
		h.t.Fatalf("write %s frame: %v", req.Op, err)
	}
	var resp fsproto.Response
	if err := fsproto.ReadFrame(h.fromHelper, &resp); err != nil {
		h.t.Fatalf("read %s response: %v", req.Op, err)
	}
	return resp
}

// wait waits for Serve to return.
func (h *helperProcess) wait() error {
	h.t.Helper()
	select {
	case err := <-h.done:
		h.done <- err // keep it observable for the cleanup path
		return err
	case <-time.After(5 * time.Second):
		h.t.Fatal("helper did not stop")
		return nil
	}
}

func requireCode(t *testing.T, resp fsproto.Response, want string) {
	t.Helper()
	if resp.OK {
		t.Fatalf("response succeeded, want %q", want)
	}
	if resp.Error == nil {
		t.Fatalf("response has no error body, want %q", want)
	}
	if resp.Error.Code != want {
		t.Fatalf("code = %q, want %q (message: %s)", resp.Error.Code, want, resp.Error.Message)
	}
}

func TestServeInitHandshake(t *testing.T) {
	h := startHelper(t)
	resp := h.initSession(fsproto.ProtocolVersion, 1024)
	if !resp.OK {
		t.Fatalf("init failed: %+v", resp.Error)
	}
	if resp.Protocol != fsproto.ProtocolVersion {
		t.Fatalf("protocol = %d, want %d", resp.Protocol, fsproto.ProtocolVersion)
	}
	if resp.HelperVersion != fsproto.HelperVersion {
		t.Fatalf("helper version = %q, want %q", resp.HelperVersion, fsproto.HelperVersion)
	}
	if resp.Root != testRoot {
		t.Fatalf("root = %q, want %q", resp.Root, testRoot)
	}
	if h.openRoot != testRoot {
		t.Fatalf("factory root = %q, want %q", h.openRoot, testRoot)
	}
	if ping := h.request(fsproto.Request{Op: fsproto.OpPing}); !ping.OK {
		t.Fatalf("ping failed: %+v", ping.Error)
	}
	if closeResp := h.request(fsproto.Request{Op: fsproto.OpClose}); !closeResp.OK {
		t.Fatalf("close failed: %+v", closeResp.Error)
	}
	if err := h.wait(); err != nil {
		t.Fatalf("Serve returned %v, want nil", err)
	}
	if h.backend.closeCalls != 1 {
		t.Fatalf("backend close calls = %d, want 1", h.backend.closeCalls)
	}
}

func TestServeProtocolMismatchIsUnsupported(t *testing.T) {
	h := startHelper(t)
	resp := h.initSession(fsproto.ProtocolVersion+1, 0)
	requireCode(t, resp, execution.FSUnsupportedFilesystem)
	if h.backend.closeCalls != 0 {
		t.Fatalf("backend was opened despite protocol mismatch")
	}
	if err := h.wait(); err != nil {
		t.Fatalf("Serve returned %v, want nil after refusing the session", err)
	}
}

func TestServeInitOpenFailureIsRelayed(t *testing.T) {
	h := startHelper(t, withOpenError(execution.NewFSError(execution.FSNotFound, testRoot, "root missing")))
	resp := h.initSession(fsproto.ProtocolVersion, 0)
	requireCode(t, resp, execution.FSNotFound)
	if resp.Error.Path != testRoot {
		t.Fatalf("path = %q, want %q", resp.Error.Path, testRoot)
	}
	if err := h.wait(); err != nil {
		t.Fatalf("Serve returned %v, want nil", err)
	}
}

func TestServeRejectsPingBeforeInit(t *testing.T) {
	h := startHelper(t)
	if err := fsproto.WriteFrame(h.toHelper, fsproto.Request{Op: fsproto.OpPing}); err != nil {
		t.Fatalf("write ping: %v", err)
	}
	if err := h.wait(); !errors.Is(err, fsproto.ErrProtocol) {
		t.Fatalf("Serve error = %v, want ErrProtocol", err)
	}
}

func TestServeReadSuccess(t *testing.T) {
	backend := &fakeBackend{target: "fake:" + testRoot}
	backend.readFn = func(name string, maxBytes int64) (execution.FileSnapshot, error) {
		if name != "a/b.txt" || maxBytes != 64 {
			t.Fatalf("Read(%q, %d), want (a/b.txt, 64)", name, maxBytes)
		}
		return execution.FileSnapshot{
			Stat:    execution.FileStat{Name: name, Version: execution.FileVersion{Size: 5, Digest: "abc"}},
			Content: []byte("hello"),
		}, nil
	}
	h := startHelper(t, withBackend(backend))
	h.initSession(fsproto.ProtocolVersion, 1024)
	resp := h.request(fsproto.Request{Op: fsproto.OpRead, Name: "a/b.txt", MaxBytes: 64})
	if !resp.OK {
		t.Fatalf("read failed: %+v", resp.Error)
	}
	if string(resp.Content) != "hello" {
		t.Fatalf("content = %q, want hello", resp.Content)
	}
	if resp.Stat == nil || resp.Stat.Version.Digest != "abc" || resp.Stat.Name != "a/b.txt" {
		t.Fatalf("stat = %+v", resp.Stat)
	}
}

func TestServeReadBackendErrorKeepsCodeAndSession(t *testing.T) {
	backend := &fakeBackend{target: "fake:" + testRoot}
	backend.readFn = func(name string, maxBytes int64) (execution.FileSnapshot, error) {
		return execution.FileSnapshot{}, execution.NewFSError(execution.FSNotFound, name, "no such file")
	}
	h := startHelper(t, withBackend(backend))
	h.initSession(fsproto.ProtocolVersion, 0)
	resp := h.request(fsproto.Request{Op: fsproto.OpRead, Name: "missing.txt", MaxBytes: 16})
	requireCode(t, resp, execution.FSNotFound)
	if resp.Error.Path != "missing.txt" {
		t.Fatalf("path = %q, want missing.txt", resp.Error.Path)
	}
	// A backend error is not a protocol error: the session stays usable.
	if ping := h.request(fsproto.Request{Op: fsproto.OpPing}); !ping.OK {
		t.Fatalf("session did not survive a backend error: %+v", ping.Error)
	}
}

func TestServeReadRespectsSessionContentBound(t *testing.T) {
	backend := &fakeBackend{target: "fake:" + testRoot}
	backend.readFn = func(name string, maxBytes int64) (execution.FileSnapshot, error) {
		return execution.FileSnapshot{Stat: execution.FileStat{Name: name}, Content: []byte("0123456789")}, nil
	}
	h := startHelper(t, withBackend(backend))
	h.initSession(fsproto.ProtocolVersion, 4)
	resp := h.request(fsproto.Request{Op: fsproto.OpRead, Name: "big.txt", MaxBytes: 1 << 20})
	requireCode(t, resp, execution.FSFileLimitExceeded)
	if len(resp.Content) != 0 {
		t.Fatalf("oversized content was returned: %q", resp.Content)
	}
}

func TestServeCreateSuccess(t *testing.T) {
	backend := &fakeBackend{target: "fake:" + testRoot}
	backend.createFn = func(name string, content []byte) (execution.FileStat, error) {
		if string(content) != "data" {
			t.Fatalf("content = %q, want data", content)
		}
		return execution.FileStat{Name: name, Version: execution.FileVersion{Size: int64(len(content))}}, nil
	}
	h := startHelper(t, withBackend(backend))
	h.initSession(fsproto.ProtocolVersion, 1024)
	resp := h.request(fsproto.Request{Op: fsproto.OpCreate, Name: "new.txt", Content: []byte("data")})
	if !resp.OK {
		t.Fatalf("create failed: %+v", resp.Error)
	}
	if resp.Stat == nil || resp.Stat.Name != "new.txt" || resp.Stat.Version.Size != 4 {
		t.Fatalf("stat = %+v", resp.Stat)
	}
}

func TestServeCreateRejectsOversizedContentWithoutSideEffect(t *testing.T) {
	backend := &fakeBackend{target: "fake:" + testRoot}
	h := startHelper(t, withBackend(backend))
	h.initSession(fsproto.ProtocolVersion, 4)
	resp := h.request(fsproto.Request{Op: fsproto.OpCreate, Name: "big.txt", Content: []byte("0123456789")})
	requireCode(t, resp, execution.FSFileLimitExceeded)
	if backend.createCalls != 0 {
		t.Fatalf("create calls = %d, want 0", backend.createCalls)
	}
}

func TestServeReplaceSuccess(t *testing.T) {
	expected := execution.FileVersion{Size: 3, Digest: "old"}
	backend := &fakeBackend{target: "fake:" + testRoot}
	backend.replaceFn = func(name string, content []byte, got execution.FileVersion) (execution.FileStat, error) {
		if !got.Equal(expected) {
			t.Fatalf("expected version = %+v, want %+v", got, expected)
		}
		return execution.FileStat{Name: name, Version: execution.FileVersion{Size: int64(len(content))}}, nil
	}
	h := startHelper(t, withBackend(backend))
	h.initSession(fsproto.ProtocolVersion, 1024)
	resp := h.request(fsproto.Request{Op: fsproto.OpReplace, Name: "a.txt", Content: []byte("new"), Expected: &expected})
	if !resp.OK {
		t.Fatalf("replace failed: %+v", resp.Error)
	}
	if resp.Stat == nil || resp.Stat.Version.Size != 3 {
		t.Fatalf("stat = %+v", resp.Stat)
	}
}

func TestServeReplaceStaleReadIsRelayed(t *testing.T) {
	expected := execution.FileVersion{Size: 3, Digest: "old"}
	backend := &fakeBackend{target: "fake:" + testRoot}
	backend.replaceFn = func(name string, content []byte, got execution.FileVersion) (execution.FileStat, error) {
		return execution.FileStat{}, execution.NewFSError(execution.FSStaleRead, name, "version changed")
	}
	h := startHelper(t, withBackend(backend))
	h.initSession(fsproto.ProtocolVersion, 0)
	resp := h.request(fsproto.Request{Op: fsproto.OpReplace, Name: "a.txt", Content: []byte("new"), Expected: &expected})
	requireCode(t, resp, execution.FSStaleRead)
}

func TestServeReplaceWithoutExpectedVersionIsProtocolError(t *testing.T) {
	backend := &fakeBackend{target: "fake:" + testRoot}
	h := startHelper(t, withBackend(backend))
	h.initSession(fsproto.ProtocolVersion, 0)
	if err := fsproto.WriteFrame(h.toHelper, fsproto.Request{Op: fsproto.OpReplace, Name: "a.txt", Content: []byte("x")}); err != nil {
		t.Fatalf("write replace: %v", err)
	}
	if err := h.wait(); !errors.Is(err, fsproto.ErrProtocol) {
		t.Fatalf("Serve error = %v, want ErrProtocol", err)
	}
	if backend.replaceCalls != 0 {
		t.Fatalf("backend replace calls = %d, want 0", backend.replaceCalls)
	}
}

func TestServeGenericBackendErrorBecomesIOError(t *testing.T) {
	backend := &fakeBackend{target: "fake:" + testRoot}
	backend.createFn = func(name string, content []byte) (execution.FileStat, error) {
		return execution.FileStat{}, errors.New("disk on fire")
	}
	h := startHelper(t, withBackend(backend))
	h.initSession(fsproto.ProtocolVersion, 0)
	resp := h.request(fsproto.Request{Op: fsproto.OpCreate, Name: "a.txt", Content: []byte("x")})
	requireCode(t, resp, execution.FSIOError)
	if resp.Error.Message != "disk on fire" {
		t.Fatalf("message = %q, want disk on fire", resp.Error.Message)
	}
}

func TestServeUnknownOpEndsSession(t *testing.T) {
	backend := &fakeBackend{target: "fake:" + testRoot}
	h := startHelper(t, withBackend(backend))
	h.initSession(fsproto.ProtocolVersion, 0)
	if err := fsproto.WriteFrame(h.toHelper, fsproto.Request{Op: fsproto.Op("explode")}); err != nil {
		t.Fatalf("write unknown op: %v", err)
	}
	if err := h.wait(); !errors.Is(err, fsproto.ErrProtocol) {
		t.Fatalf("Serve error = %v, want ErrProtocol", err)
	}
}

func TestServeSecondInitIsProtocolError(t *testing.T) {
	h := startHelper(t)
	h.initSession(fsproto.ProtocolVersion, 0)
	if err := fsproto.WriteFrame(h.toHelper, fsproto.Request{Op: fsproto.OpInit, Protocol: fsproto.ProtocolVersion, Root: testRoot}); err != nil {
		t.Fatalf("write second init: %v", err)
	}
	if err := h.wait(); !errors.Is(err, fsproto.ErrProtocol) {
		t.Fatalf("Serve error = %v, want ErrProtocol", err)
	}
}

func TestServeHangupEndsCleanly(t *testing.T) {
	backend := &fakeBackend{target: "fake:" + testRoot}
	h := startHelper(t, withBackend(backend))
	h.initSession(fsproto.ProtocolVersion, 0)
	if err := h.toHelper.Close(); err != nil {
		t.Fatalf("close request stream: %v", err)
	}
	if err := h.wait(); err != nil {
		t.Fatalf("Serve error = %v, want nil", err)
	}
	if backend.closeCalls != 1 {
		t.Fatalf("backend close calls = %d, want 1", backend.closeCalls)
	}
}
