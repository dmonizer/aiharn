package ssh

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"aiharn/internal/execution"
	"aiharn/internal/logging"
)

// killGrace is how long Exec waits for a terminated command to unwind before
// escalating from SIGTERM to SIGKILL.
const killGrace = 3 * time.Second

// Session implements execution.Session over one persistent non-PTY shell.
// It is not safe for concurrent use; the agent serializes access.
type Session struct {
	client    *ssh.Client
	shell     *ssh.Session
	stdin     io.WriteCloser
	reader    *bufio.Reader
	opts      Options
	dedicated bool
	onDead    func() // invoked once when the session becomes unusable (shared client)

	mu        sync.Mutex // serializes Exec
	closeOnce sync.Once
	deadMu    sync.Mutex
	dead      bool
}

var _ execution.Session = (*Session)(nil)

func openShellSession(ctx context.Context, client *ssh.Client, opts Options, dedicated bool, onDead func()) (*Session, error) {
	interp := interpreterCommand(opts)
	logging.Debug("ssh: openShellSession new session",
		slog.String("component", "ssh"),
		slog.String("shell", opts.DefaultShell),
		slog.String("command", interp),
		slog.Bool("dedicated", dedicated),
	)
	sess, err := newSSHSession(ctx, client)
	if err != nil {
		logging.Debug("ssh: openShellSession new session failed", slog.String("component", "ssh"), slog.Any("err", err))
		return nil, fmt.Errorf("ssh: open session: %w", err)
	}
	stdin, err := sess.StdinPipe()
	if err != nil {
		logging.Debug("ssh: openShellSession stdin pipe failed", slog.String("component", "ssh"), slog.Any("err", err))
		sess.Close()
		return nil, fmt.Errorf("ssh: stdin pipe: %w", err)
	}
	stdout, err := sess.StdoutPipe()
	if err != nil {
		logging.Debug("ssh: openShellSession stdout pipe failed", slog.String("component", "ssh"), slog.Any("err", err))
		sess.Close()
		return nil, fmt.Errorf("ssh: stdout pipe: %w", err)
	}
	stderr, err := sess.StderrPipe()
	if err != nil {
		logging.Debug("ssh: openShellSession stderr pipe failed", slog.String("component", "ssh"), slog.Any("err", err))
		sess.Close()
		return nil, fmt.Errorf("ssh: stderr pipe: %w", err)
	}
	for _, e := range opts.Env {
		name, value, ok := strings.Cut(e, "=")
		if !ok || name == "" {
			continue
		}
		if err := sess.Setenv(name, value); err != nil {
			logging.Debug("ssh: openShellSession setenv rejected", slog.String("component", "ssh"), slog.String("name", name), slog.Any("err", err))
		}
	}
	if err := sess.Start(interp); err != nil {
		logging.Debug("ssh: openShellSession start shell failed",
			slog.String("component", "ssh"),
			slog.String("command", interp),
			slog.Any("err", err),
		)
		sess.Close()
		return nil, fmt.Errorf("ssh: start shell: %w", err)
	}
	logging.Debug("ssh: openShellSession ok", slog.String("component", "ssh"), slog.String("command", interp))

	// Drain shell-level stderr (rare: startup errors, missing setsid/base64) so
	// it cannot backpressure the shell. Command stderr never flows here; the
	// framing protocol captures it via a temp file.
	go func() { _, _ = io.Copy(io.Discard, stderr) }()

	return &Session{
		client:    client,
		shell:     sess,
		stdin:     stdin,
		reader:    bufio.NewReader(stdout),
		opts:      opts,
		dedicated: dedicated,
		onDead:    onDead,
	}, nil
}

type sshSessionResult struct {
	session *ssh.Session
	err     error
}

// newSSHSession makes ssh.Client.NewSession cancellation-aware. If the SSH
// request completes after cancellation, the late session is closed instead of
// being leaked.
func newSSHSession(ctx context.Context, client *ssh.Client) (*ssh.Session, error) {
	done := make(chan sshSessionResult)
	go func() {
		session, err := client.NewSession()
		select {
		case done <- sshSessionResult{session: session, err: err}:
		case <-ctx.Done():
			if session != nil {
				_ = session.Close()
			}
		}
	}()
	select {
	case result := <-done:
		if err := ctx.Err(); err != nil {
			if result.session != nil {
				_ = result.session.Close()
			}
			return nil, err
		}
		return result.session, result.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (s *Session) markDead() {
	s.deadMu.Lock()
	already := s.dead
	s.dead = true
	s.deadMu.Unlock()
	logging.Debug("ssh: markDead", slog.String("component", "ssh"), slog.Bool("already_dead", already))
	if !already && s.onDead != nil {
		s.onDead()
	}
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
	logging.Debug("ssh: Exec start",
		slog.String("component", "ssh"),
		slog.String("cmd", cmd),
		slog.String("cwd", opts.Cwd),
		slog.Int64("max_output", opts.MaxOutputBytes),
	)

	if s.isDead() {
		err := fmt.Errorf("%w: session is no longer usable", execution.ErrSessionReset)
		logging.Debug("ssh: Exec fail", slog.String("component", "ssh"), slog.Int64("duration_ms", time.Since(start).Milliseconds()), slog.Any("err", err))
		return execution.Result{}, err
	}

	m, err := newMarkers()
	if err != nil {
		logging.Debug("ssh: Exec fail", slog.String("component", "ssh"), slog.Int64("duration_ms", time.Since(start).Milliseconds()), slog.Any("err", err))
		return execution.Result{}, err
	}
	script := buildWrapperScript(m, opts.Cwd, cmd)

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
		logging.Debug("ssh: Exec fail", slog.String("component", "ssh"), slog.Int64("duration_ms", time.Since(start).Milliseconds()), slog.Any("err", werr))
		return execution.Result{}, werr
	case <-ctx.Done():
		s.Close()
		cerr := fmt.Errorf("%w: cancelled while writing command: %v", execution.ErrSessionReset, ctx.Err())
		logging.Debug("ssh: Exec fail", slog.String("component", "ssh"), slog.Int64("duration_ms", time.Since(start).Milliseconds()), slog.Any("err", cerr))
		return execution.Result{}, cerr
	}

	type beginResult struct {
		pid int
		err error
	}
	beginDone := make(chan beginResult, 1)
	go func() {
		pid, err := readBegin(s.reader, m)
		beginDone <- beginResult{pid: pid, err: err}
	}()
	var pid int
	select {
	case r := <-beginDone:
		pid, err = r.pid, r.err
	case <-ctx.Done():
		s.Close()
		cerr := fmt.Errorf("%w: cancelled before command started: %v", execution.ErrSessionReset, ctx.Err())
		logging.Debug("ssh: Exec fail", slog.String("component", "ssh"), slog.Int64("duration_ms", time.Since(start).Milliseconds()), slog.Any("err", cerr))
		return execution.Result{}, cerr
	}
	if err != nil {
		if errors.Is(err, execution.ErrSessionReset) {
			s.closeDead()
		} else {
			s.Close()
		}
		berr := fmt.Errorf("ssh: persistent shell failed before producing output (is %q the right shell on the remote host?): %w", interpreterCommand(s.opts), err)
		logging.Debug("ssh: Exec fail", slog.String("component", "ssh"), slog.Int64("duration_ms", time.Since(start).Milliseconds()), slog.Any("err", berr))
		return execution.Result{}, berr
	}

	type bodyResult struct {
		f   execFrame
		err error
	}
	ch := make(chan bodyResult, 1)
	go func() {
		f, err := readBody(s.reader, m, opts.MaxOutputBytes, opts.Stream)
		ch <- bodyResult{f, err}
	}()

	for {
		select {
		case r := <-ch:
			res, err := s.finishBody(r.f, r.err)
			s.logExecFinish(start, pid, res, err)
			return res, err
		case <-ctx.Done():
			for _, sig := range []string{"TERM", "KILL"} {
				logging.Debug("ssh: Exec cancellation signal",
					slog.String("component", "ssh"),
					slog.String("signal", sig),
					slog.Int("pid", pid),
					slog.Int("pgid", pid),
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
			logging.Debug("ssh: Exec finish",
				slog.String("component", "ssh"),
				slog.Int("pid", pid),
				slog.Int("pgid", pid),
				slog.Int64("duration_ms", time.Since(start).Milliseconds()),
				slog.Any("err", serr),
			)
			return execution.Result{}, serr
		}
	}
}

// logExecFinish records the outcome of a completed Exec: the remote process
// group id, exit code, retained byte counts, truncation, duration, and any
// error. It never logs output contents.
func (s *Session) logExecFinish(start time.Time, pid int, res execution.Result, err error) {
	attrs := []slog.Attr{
		slog.String("component", "ssh"),
		slog.Int("pid", pid),
		slog.Int("pgid", pid),
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
	logging.Debug("ssh: Exec finish", attrs...)
}

func (s *Session) finishBody(f execFrame, err error) (execution.Result, error) {
	if errors.Is(err, execution.ErrSessionReset) {
		s.closeDead()
	} else if err != nil {
		var streamErr *streamWriteError
		if !errors.As(err, &streamErr) {
			s.Close()
		}
	}
	return s.toResult(f, err)
}

func (s *Session) toResult(f execFrame, err error) (execution.Result, error) {
	if err != nil {
		return execution.Result{}, err
	}
	return execution.Result{
		Stdout:    string(f.stdout),
		Stderr:    string(f.stderr),
		ExitCode:  f.exitCode,
		Truncated: f.truncated,
	}, nil
}

// signal sends sig to the command's remote process group via a throwaway
// channel on the same client connection. It is best-effort and bounded.
func (s *Session) signal(pid int, sig string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	sess, err := newSSHSession(ctx, s.client)
	if err != nil {
		return
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer sess.Close()
		_ = sess.Run(fmt.Sprintf("kill -%s -- -%d 2>/dev/null || true", sig, pid))
	}()
	select {
	case <-done:
	case <-ctx.Done():
		sess.Close()
		<-done
	}
}

// Close terminates the shell and (for a dedicated connection) its client. It is
// idempotent and does not block on an in-flight Exec.
func (s *Session) Close() error {
	s.closeOnce.Do(func() {
		s.markClosed()
		logging.Debug("ssh: Session.Close", slog.String("component", "ssh"), slog.Bool("dedicated", s.dedicated))
		if s.shell != nil {
			s.shell.Close()
		}
		if s.dedicated && s.client != nil {
			s.client.Close()
		}
	})
	return nil
}

func (s *Session) closeDead() {
	logging.Debug("ssh: closeDead", slog.String("component", "ssh"))
	s.markDead()
	_ = s.Close()
}

// interpreterCommand returns the remote command that starts the persistent
// command interpreter. When RemoteCommand is set it is used verbatim (e.g.
// "bash"), letting the user force a clean shell and bypass whatever the remote
// host would otherwise run (a login shell, or a screen/tmux wrapper that
// disturbs the non-PTY command stream). Otherwise the configured default shell
// is started in stdin-reading, profile-less mode.
func interpreterCommand(opts Options) string {
	if opts.RemoteCommand != "" {
		return opts.RemoteCommand
	}
	return shellCommand(opts.DefaultShell)
}

// shellCommand builds the remote command that starts the persistent shell. The
// shell reads commands from stdin and never exits until the channel closes.
func shellCommand(shell string) string {
	quoted := shellQuote(shell)
	if strings.HasSuffix(shell, "bash") {
		return quoted + " --noprofile --norc -s"
	}
	return quoted + " -s"
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
