package ssh

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"aiharn/internal/execution"
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
	sess, err := newSSHSession(ctx, client)
	if err != nil {
		return nil, fmt.Errorf("ssh: open session: %w", err)
	}
	stdin, err := sess.StdinPipe()
	if err != nil {
		sess.Close()
		return nil, fmt.Errorf("ssh: stdin pipe: %w", err)
	}
	stdout, err := sess.StdoutPipe()
	if err != nil {
		sess.Close()
		return nil, fmt.Errorf("ssh: stdout pipe: %w", err)
	}
	stderr, err := sess.StderrPipe()
	if err != nil {
		sess.Close()
		return nil, fmt.Errorf("ssh: stderr pipe: %w", err)
	}
	if err := sess.Start(shellCommand(opts.DefaultShell)); err != nil {
		sess.Close()
		return nil, fmt.Errorf("ssh: start shell: %w", err)
	}

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

	if s.isDead() {
		return execution.Result{}, fmt.Errorf("%w: session is no longer usable", execution.ErrSessionReset)
	}

	m, err := newMarkers()
	if err != nil {
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
		return execution.Result{}, fmt.Errorf("%w: write: %v", execution.ErrSessionReset, err)
	case <-ctx.Done():
		s.Close()
		return execution.Result{}, fmt.Errorf("%w: cancelled while writing command: %v", execution.ErrSessionReset, ctx.Err())
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
		return execution.Result{}, fmt.Errorf("%w: cancelled before command started: %v", execution.ErrSessionReset, ctx.Err())
	}
	if err != nil {
		if errors.Is(err, execution.ErrSessionReset) {
			s.closeDead()
		} else {
			s.Close()
		}
		return execution.Result{}, fmt.Errorf("ssh: persistent shell failed before producing output (is %q the right shell on the remote host?): %w", s.opts.DefaultShell, err)
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
			return s.finishBody(r.f, r.err)
		case <-ctx.Done():
			for _, sig := range []string{"TERM", "KILL"} {
				s.signal(pid, sig)
				select {
				case r := <-ch:
					return s.finishBody(r.f, r.err)
				case <-time.After(killGrace):
				}
			}
			s.Close()
			return execution.Result{}, fmt.Errorf("%w: command survived TERM and KILL", execution.ErrSessionReset)
		}
	}
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
	s.markDead()
	_ = s.Close()
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
