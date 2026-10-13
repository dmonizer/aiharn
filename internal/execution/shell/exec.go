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

// ExecHooks carries the transport-specific operations the shared Exec runner
// needs. Component names the transport in debug logs ("local" or "ssh").
type ExecHooks struct {
	Component string
	Stdin     io.Writer
	Reader    *bufio.Reader

	// IsDead reports whether the session is no longer usable.
	IsDead func() bool
	// OnReset marks the session dead and closes it after a stream reset.
	OnReset func()
	// OnCancel closes the session after cancellation or a non-reset failure.
	OnCancel func()
	// Signal sends the named signal ("TERM" or "KILL") to the command's
	// process group. Best-effort.
	Signal func(pid int, name string)
	// ShellName returns the interpreter name used in error messages.
	ShellName func() string
	// BeforeOutputErr builds the error reported when the shell dies before
	// producing framed output.
	BeforeOutputErr func(shellName string, err error) error
	// LogFinish records the outcome of a completed Exec.
	LogFinish func(start time.Time, pid int, res execution.Result, err error)
}

// Exec runs one framed command over the persistent shell stream described by h.
// The caller must serialize concurrent Exec calls (each transport's Session
// holds a mutex). This is the shared implementation used by the local and ssh
// transports; their only differences are the process signaling, interpreter
// naming, and debug logging supplied through h.
func Exec(ctx context.Context, cmd string, opts execution.ExecOptions, h ExecHooks) (execution.Result, error) {
	start := time.Now()
	logging.Debug(h.Component+": Exec start",
		slog.String("component", h.Component),
		slog.String("cmd", cmd),
		slog.String("cwd", opts.Cwd),
		slog.Int64("max_output", opts.MaxOutputBytes),
	)

	if h.IsDead() {
		err := fmt.Errorf("%w: session is no longer usable", execution.ErrSessionReset)
		logExecFail(h.Component, start, err)
		return execution.Result{}, err
	}

	m, err := NewMarkers()
	if err != nil {
		logExecFail(h.Component, start, err)
		return execution.Result{}, err
	}
	script := BuildWrapperScript(m, opts.Cwd, cmd)

	writeDone := make(chan error, 1)
	go func() {
		_, err := io.WriteString(h.Stdin, script)
		writeDone <- err
	}()
	select {
	case err := <-writeDone:
		if err == nil {
			break
		}
		h.OnReset()
		werr := fmt.Errorf("%w: write: %v", execution.ErrSessionReset, err)
		logExecFail(h.Component, start, werr)
		return execution.Result{}, werr
	case <-ctx.Done():
		h.OnCancel()
		cerr := fmt.Errorf("%w: cancelled while writing command: %v", execution.ErrSessionReset, ctx.Err())
		logExecFail(h.Component, start, cerr)
		return execution.Result{}, cerr
	}

	type beginResult struct {
		pid int
		err error
	}
	beginDone := make(chan beginResult, 1)
	go func() {
		pid, err := ReadBegin(h.Reader, m)
		beginDone <- beginResult{pid: pid, err: err}
	}()
	var pid int
	select {
	case r := <-beginDone:
		pid, err = r.pid, r.err
	case <-ctx.Done():
		h.OnCancel()
		cerr := fmt.Errorf("%w: cancelled before command started: %v", execution.ErrSessionReset, ctx.Err())
		logExecFail(h.Component, start, cerr)
		return execution.Result{}, cerr
	}
	if err != nil {
		if errors.Is(err, execution.ErrSessionReset) {
			h.OnReset()
		} else {
			h.OnCancel()
		}
		berr := h.BeforeOutputErr(h.ShellName(), err)
		logExecFail(h.Component, start, berr)
		return execution.Result{}, berr
	}

	type bodyResult struct {
		f   Frame
		err error
	}
	ch := make(chan bodyResult, 1)
	go func() {
		f, err := ReadBody(h.Reader, m, opts.MaxOutputBytes, opts.Stream)
		ch <- bodyResult{f, err}
	}()

	for {
		select {
		case r := <-ch:
			res, err := finishBody(h, r.f, r.err)
			h.LogFinish(start, pid, res, err)
			return res, err
		case <-ctx.Done():
			for _, sig := range []string{"TERM", "KILL"} {
				logging.Debug(h.Component+": Exec cancellation signal",
					slog.String("component", h.Component),
					slog.String("signal", sig),
					slog.Int("pid", pid),
				)
				h.Signal(pid, sig)
				select {
				case r := <-ch:
					res, err := finishBody(h, r.f, r.err)
					h.LogFinish(start, pid, res, err)
					return res, err
				case <-time.After(killGrace):
				}
			}
			h.OnCancel()
			serr := fmt.Errorf("%w: command survived TERM and KILL", execution.ErrSessionReset)
			h.LogFinish(start, pid, execution.Result{}, serr)
			return execution.Result{}, serr
		}
	}
}

// finishBody maps a completed body read onto a Result, closing the session on
// a stream reset and on any non-stream error.
func finishBody(h ExecHooks, f Frame, err error) (execution.Result, error) {
	if errors.Is(err, execution.ErrSessionReset) {
		h.OnReset()
	} else if err != nil {
		var streamErr *StreamWriteError
		if !errors.As(err, &streamErr) {
			h.OnCancel()
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
