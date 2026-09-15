package local

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"aiharn/internal/execution"
	"aiharn/internal/execution/shell"
	"aiharn/internal/logging"
)

// killGrace is how long Exec waits for a terminated command to unwind before
// escalating from SIGTERM to SIGKILL.
const killGrace = 3 * time.Second

// Session implements execution.Session over one persistent local shell.
type Session struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	reader *bufio.Reader
	opts   Options

	mu        sync.Mutex // serializes Exec
	closeOnce sync.Once
	deadMu    sync.Mutex
	dead      bool
}

var _ execution.Session = (*Session)(nil)

func openSession(opts Options) (*Session, error) {
	cmd, stdin, reader, err := startShell(opts)
	if err != nil {
		return nil, err
	}
	return &Session{
		cmd:    cmd,
		stdin:  stdin,
		reader: reader,
		opts:   opts,
	}, nil
}

// startShell launches the persistent local shell with stdin/stdout pipes, in a
// new session so it and its descendants can be killed as one process group.
func startShell(opts Options) (*exec.Cmd, io.WriteCloser, *bufio.Reader, error) {
	var cmd *exec.Cmd
	if opts.RemoteCommand != "" {
		cmd = exec.Command("/bin/sh", "-c", opts.RemoteCommand)
	} else {
		cmd = exec.Command(opts.DefaultShell, shell.ShellArgs(opts.DefaultShell)...)
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("local: stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("local: stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("local: stderr pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, nil, nil, fmt.Errorf("local: start shell %q: %w", shellName(opts), err)
	}
	logging.Debug("local: startShell ok", slog.String("component", "local"), slog.String("shell", shellName(opts)))

	// Drain shell-level stderr (rare: startup errors) so it cannot backpressure
	// the shell. Command stderr never flows here; the framing protocol captures
	// it via a temp file.
	go func() { _, _ = io.Copy(io.Discard, stderr) }()

	return cmd, stdin, bufio.NewReader(stdout), nil
}

func shellName(opts Options) string {
	if opts.RemoteCommand != "" {
		return opts.RemoteCommand
	}
	return opts.DefaultShell
}

func (s *Session) markDead() {
	s.deadMu.Lock()
	already := s.dead
	s.dead = true
	s.deadMu.Unlock()
	logging.Debug("local: markDead", slog.String("component", "local"), slog.Bool("already_dead", already))
}

func (s *Session) markClosed() {
	s.deadMu.Lock()
	s.dead = true
	s.deadMu.Unlock()
}

func (s *Session) isDead() bool {
	s.deadMu.Lock()
	defer s.deadMu.Unlock()
	return s.dead
}

// Exec implements execution.Session.
func (s *Session) Exec(ctx context.Context, cmd string, opts execution.ExecOptions) (execution.Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	start := time.Now()
	logging.Debug("local: Exec start",
		slog.String("component", "local"),
		slog.String("cmd", cmd),
		slog.String("cwd", opts.Cwd),
		slog.Int64("max_output", opts.MaxOutputBytes),
	)

	if s.isDead() {
		err := fmt.Errorf("%w: session is no longer usable", execution.ErrSessionReset)
		logging.Debug("local: Exec fail", slog.String("component", "local"), slog.Int64("duration_ms", time.Since(start).Milliseconds()), slog.Any("err", err))
		return execution.Result{}, err
	}

	m, err := shell.NewMarkers()
	if err != nil {
		logging.Debug("local: Exec fail", slog.String("component", "local"), slog.Int64("duration_ms", time.Since(start).Milliseconds()), slog.Any("err", err))
		return execution.Result{}, err
	}
	script := shell.BuildWrapperScript(m, opts.Cwd, cmd)

	writeDone := make(chan error, 1)
	go func() {
		_, err := io.WriteString(s.stdin, script)
		writeDone <- err
	}()
	select {
	case err := <-writeDone:
		if err == nil {
			break
		}
		s.closeDead()
		werr := fmt.Errorf("%w: write: %v", execution.ErrSessionReset, err)
		logging.Debug("local: Exec fail", slog.String("component", "local"), slog.Int64("duration_ms", time.Since(start).Milliseconds()), slog.Any("err", werr))
		return execution.Result{}, werr
	case <-ctx.Done():
		s.Close()
		cerr := fmt.Errorf("%w: cancelled while writing command: %v", execution.ErrSessionReset, ctx.Err())
		logging.Debug("local: Exec fail", slog.String("component", "local"), slog.Int64("duration_ms", time.Since(start).Milliseconds()), slog.Any("err", cerr))
		return execution.Result{}, cerr
	}

	type beginResult struct {
		pid int
		err error
	}
	beginDone := make(chan beginResult, 1)
	go func() {
		pid, err := shell.ReadBegin(s.reader, m)
		beginDone <- beginResult{pid: pid, err: err}
	}()
	var pid int
	select {
	case r := <-beginDone:
		pid, err = r.pid, r.err
	case <-ctx.Done():
		s.Close()
		cerr := fmt.Errorf("%w: cancelled before command started: %v", execution.ErrSessionReset, ctx.Err())
		logging.Debug("local: Exec fail", slog.String("component", "local"), slog.Int64("duration_ms", time.Since(start).Milliseconds()), slog.Any("err", cerr))
		return execution.Result{}, cerr
	}
	if err != nil {
		if errors.Is(err, execution.ErrSessionReset) {
			s.closeDead()
		} else {
			s.Close()
		}
		berr := fmt.Errorf("local: persistent shell failed before producing output (is %q a valid local shell?): %w", shellName(s.opts), err)
		logging.Debug("local: Exec fail", slog.String("component", "local"), slog.Int64("duration_ms", time.Since(start).Milliseconds()), slog.Any("err", berr))
		return execution.Result{}, berr
	}

	type bodyResult struct {
		f   shell.Frame
		err error
	}
	ch := make(chan bodyResult, 1)
	go func() {
		f, err := shell.ReadBody(s.reader, m, opts.MaxOutputBytes, opts.Stream)
		ch <- bodyResult{f, err}
	}()

	for {
		select {
		case r := <-ch:
			res, err := s.finishBody(r.f, r.err)
			s.logExecFinish(start, pid, res, err)
			return res, err
		case <-ctx.Done():
			for _, sig := range []syscall.Signal{syscall.SIGTERM, syscall.SIGKILL} {
				logging.Debug("local: Exec cancellation signal",
					slog.String("component", "local"),
					slog.Int64("signal", int64(sig)),
					slog.Int("pid", pid),
				)
				s.signal(pid, sig)
				select {
				case r := <-ch:
					res, err := s.finishBody(r.f, r.err)
					s.logExecFinish(start, pid, res, err)
					return res, err
				case <-time.After(killGrace):
				}
			}
			s.Close()
			serr := fmt.Errorf("%w: command survived TERM and KILL", execution.ErrSessionReset)
			logging.Debug("local: Exec finish",
				slog.String("component", "local"),
				slog.Int("pid", pid),
				slog.Int64("duration_ms", time.Since(start).Milliseconds()),
				slog.Any("err", serr),
			)
			return execution.Result{}, serr
		}
	}
}

func (s *Session) logExecFinish(start time.Time, pid int, res execution.Result, err error) {
	attrs := []slog.Attr{
		slog.String("component", "local"),
		slog.Int("pid", pid),
		slog.Int64("duration_ms", time.Since(start).Milliseconds()),
	}
	if err != nil {
		attrs = append(attrs, slog.Any("err", err))
	} else {
		attrs = append(attrs,
			slog.Int("exit_code", res.ExitCode),
			slog.Int("stdout_bytes", len(res.Stdout)),
			slog.Int("stderr_bytes", len(res.Stderr)),
			slog.Bool("truncated", res.Truncated),
		)
	}
	logging.Debug("local: Exec finish", attrs...)
}

func (s *Session) finishBody(f shell.Frame, err error) (execution.Result, error) {
	if errors.Is(err, execution.ErrSessionReset) {
		s.closeDead()
	} else if err != nil {
		var streamErr *shell.StreamWriteError
		if !errors.As(err, &streamErr) {
			s.Close()
		}
	}
	return s.toResult(f, err)
}

func (s *Session) toResult(f shell.Frame, err error) (execution.Result, error) {
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

// signal sends sig to the command's local process group. It is best-effort.
func (s *Session) signal(pid int, sig syscall.Signal) {
	if pid <= 0 {
		return
	}
	_ = syscall.Kill(-pid, sig)
}

// Close terminates the persistent shell. It is idempotent and does not block on
// an in-flight Exec.
func (s *Session) Close() error {
	s.closeOnce.Do(func() {
		s.markClosed()
		logging.Debug("local: Session.Close", slog.String("component", "local"))
		if s.stdin != nil {
			_ = s.stdin.Close()
		}
		s.killAndWait()
	})
	return nil
}

func (s *Session) closeDead() {
	logging.Debug("local: closeDead", slog.String("component", "local"))
	s.markDead()
	_ = s.Close()
}

// killAndWait reaps the shell process. Closing stdin normally makes the shell
// exit on EOF; if it lingers, the process group is TERM'd then KILL'd.
func (s *Session) killAndWait() {
	if s.cmd == nil || s.cmd.Process == nil {
		return
	}
	done := make(chan struct{})
	go func() { _ = s.cmd.Wait(); close(done) }()
	select {
	case <-done:
		return
	case <-time.After(killGrace):
		_ = syscall.Kill(-s.cmd.Process.Pid, syscall.SIGTERM)
		select {
		case <-done:
			return
		case <-time.After(killGrace):
			_ = syscall.Kill(-s.cmd.Process.Pid, syscall.SIGKILL)
			<-done
		}
	}
}
