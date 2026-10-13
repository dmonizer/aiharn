package shell

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"aiharn/internal/execution"
	"aiharn/internal/logging"
)

// killGrace is how long Exec waits for a terminated command to unwind before
// escalating from SIGTERM to SIGKILL.
const killGrace = 3 * time.Second

// Stream is the persistent-shell stream a transport owns. The shared Exec
// runner reads/writes the stream and drives the transport's lifecycle and
// signaling through these methods.
type Stream interface {
	Stdin() io.Writer
	Reader() *bufio.Reader
	IsDead() bool
	// Reset marks the session dead and closes it after a stream reset.
	Reset()
	// Close terminates the session (cancellation / non-reset failures).
	Close() error
	// Signal sends the named signal ("TERM" or "KILL") to the command's
	// process group. Best-effort.
	Signal(pid int, name string)
}

// ExecConfig carries the transport-specific details the shared Exec runner
// needs beyond the stream itself: the debug log component name, the interpreter
// name used in errors, and the per-transport error/log formatting.
type ExecConfig struct {
	Component       string
	ShellName       func() string
	BeforeOutputErr func(shellName string, err error) error
	LogFinish       func(start time.Time, pid int, res execution.Result, err error)
	// KillGrace overrides how long Exec waits between escalation signals; zero
	// uses the default (killGrace).
	KillGrace time.Duration
}

// Exec runs one framed command over the persistent shell stream described by
// stream. The caller must serialize concurrent Exec calls (each transport's
// Session holds a mutex). This is the shared implementation used by the local
// and ssh transports; their only differences are the process signaling,
// interpreter naming, and debug logging supplied through cfg.
func Exec(ctx context.Context, cmd string, opts execution.ExecOptions, stream Stream, cfg ExecConfig) (execution.Result, error) {
	component := cfg.Component
	grace := cfg.KillGrace
	if grace <= 0 {
		grace = killGrace
	}
	start := time.Now()
	logging.Debug(component+": Exec start",
		slog.String("component", component),
		slog.String("cmd", cmd),
		slog.String("cwd", opts.Cwd),
		slog.Int64("max_output", opts.MaxOutputBytes),
	)

	if stream.IsDead() {
		err := fmt.Errorf("%w: session is no longer usable", execution.ErrSessionReset)
		logExecFail(component, start, err)
		return execution.Result{}, err
	}

	m, err := NewMarkers()
	if err != nil {
		logExecFail(component, start, err)
		return execution.Result{}, err
	}
	script := BuildWrapperScript(m, opts.Cwd, cmd)

	writeDone := make(chan error, 1)
	go func() {
		_, err := io.WriteString(stream.Stdin(), script)
		writeDone <- err
	}()
	select {
	case err := <-writeDone:
		if err == nil {
			break
		}
		stream.Reset()
		werr := fmt.Errorf("%w: write: %v", execution.ErrSessionReset, err)
		logExecFail(component, start, werr)
		return execution.Result{}, werr
	case <-ctx.Done():
		_ = stream.Close()
		cerr := fmt.Errorf("%w: cancelled while writing command: %v", execution.ErrSessionReset, ctx.Err())
		logExecFail(component, start, cerr)
		return execution.Result{}, cerr
	}

	type beginResult struct {
		pid int
		err error
	}
	beginDone := make(chan beginResult, 1)
	go func() {
		pid, err := ReadBegin(stream.Reader(), m)
		beginDone <- beginResult{pid: pid, err: err}
	}()
	var pid int
	select {
	case r := <-beginDone:
		pid, err = r.pid, r.err
	case <-ctx.Done():
		_ = stream.Close()
		cerr := fmt.Errorf("%w: cancelled before command started: %v", execution.ErrSessionReset, ctx.Err())
		logExecFail(component, start, cerr)
		return execution.Result{}, cerr
	}
	if err != nil {
		if errors.Is(err, execution.ErrSessionReset) {
			stream.Reset()
		} else {
			_ = stream.Close()
		}
		berr := cfg.BeforeOutputErr(cfg.ShellName(), err)
		logExecFail(component, start, berr)
		return execution.Result{}, berr
	}

	type bodyResult struct {
		f   Frame
		err error
	}
	ch := make(chan bodyResult, 1)
	go func() {
		f, err := ReadBody(stream.Reader(), m, opts.MaxOutputBytes, opts.Stream)
		ch <- bodyResult{f, err}
	}()

	for {
		select {
		case r := <-ch:
			res, err := finishBody(stream, r.f, r.err)
			cfg.LogFinish(start, pid, res, err)
			return res, err
		case <-ctx.Done():
			for _, sig := range []string{"TERM", "KILL"} {
				logging.Debug(component+": Exec cancellation signal",
					slog.String("component", component),
					slog.String("signal", sig),
					slog.Int("pid", pid),
				)
				stream.Signal(pid, sig)
				select {
				case r := <-ch:
					res, err := finishBody(stream, r.f, r.err)
					cfg.LogFinish(start, pid, res, err)
					return res, err
				case <-time.After(grace):
				}
			}
			_ = stream.Close()
			serr := fmt.Errorf("%w: command survived TERM and KILL", execution.ErrSessionReset)
			cfg.LogFinish(start, pid, execution.Result{}, serr)
			return execution.Result{}, serr
		}
	}
}

// finishBody maps a completed body read onto a Result, resetting the session on
// a stream reset and closing it on any non-stream error.
func finishBody(stream Stream, f Frame, err error) (execution.Result, error) {
	if errors.Is(err, execution.ErrSessionReset) {
		stream.Reset()
	} else if err != nil {
		var streamErr *StreamWriteError
		if !errors.As(err, &streamErr) {
			_ = stream.Close()
		}
	}
	return toResult(f, err)
}

func toResult(f Frame, err error) (execution.Result, error) {
	if err != nil {
		return execution.Result{}, err
	}
	return execution.Result{
		Stdout:    string(f.Stdout),
		Stderr:    string(f.Stderr),
		ExitCode:  f.ExitCode,
		Truncated: f.Truncated,
	}, nil
}

func logExecFail(component string, start time.Time, err error) {
	logging.Debug(component+": Exec fail",
		slog.String("component", component),
		slog.Int64("duration_ms", time.Since(start).Milliseconds()),
		slog.Any("err", err),
	)
}
