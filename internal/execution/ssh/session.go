package ssh

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"aiharn/internal/execution"
	"aiharn/internal/execution/shell"
	"aiharn/internal/logging"
)

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

// Stdin returns the persistent shell's stdin stream.
func (s *Session) Stdin() io.Writer { return s.stdin }

// Reader returns the persistent shell's framed stdout stream.
func (s *Session) Reader() *bufio.Reader { return s.reader }

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

func (s *Session) IsDead() bool {
	s.deadMu.Lock()
	defer s.deadMu.Unlock()
	return s.dead
}

// Exec implements execution.Session.
func (s *Session) Exec(ctx context.Context, cmd string, opts execution.ExecOptions) (execution.Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return shell.Exec(ctx, cmd, opts, s, shell.ExecConfig{
		Component: "ssh",
		ShellName: func() string { return interpreterCommand(s.opts) },
		BeforeOutputErr: func(name string, err error) error {
			return fmt.Errorf("ssh: persistent shell failed before producing output (is %q the right shell on the remote host?): %w", name, err)
		},
		LogFinish: s.logExecFinish,
	})
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

// signal sends sig to the command's remote process group via a throwaway
// channel on the same client connection. It is best-effort and bounded.
func (s *Session) Signal(pid int, name string) {
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
		_ = sess.Run(fmt.Sprintf("kill -%s -- -%d 2>/dev/null || true", name, pid))
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

func (s *Session) Reset() {
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
	return shell.ShellCommand(opts.DefaultShell)
}
