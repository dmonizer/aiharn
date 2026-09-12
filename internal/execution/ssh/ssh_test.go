package ssh_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"aiharn/internal/execution"
	execssh "aiharn/internal/execution/ssh"
	"aiharn/internal/execution/ssh/harness"
)

func newServer(t *testing.T) *harness.Server {
	t.Helper()
	srv, err := harness.New("/bin/bash")
	if err != nil {
		t.Fatalf("harness.New: %v", err)
	}
	t.Cleanup(func() { srv.Close() })
	return srv
}

func newTransport(t *testing.T, srv *harness.Server, opts execssh.Options) *execssh.Transport {
	t.Helper()
	if opts.Host == "" {
		opts.Host = "127.0.0.1"
	}
	if opts.Port == 0 {
		opts.Port = srv.Port()
	}
	if opts.User == "" {
		opts.User = "test"
	}
	if opts.Password == "" && opts.KeyFile == "" {
		opts.Password = "test"
	}
	if opts.DefaultShell == "" {
		opts.DefaultShell = "/bin/bash"
	}
	tr, err := execssh.NewTransport(opts)
	if err != nil {
		t.Fatalf("NewTransport: %v", err)
	}
	t.Cleanup(func() { tr.Close() })
	return tr
}

func openSession(t *testing.T, tr *execssh.Transport) execution.Session {
	t.Helper()
	s, err := tr.NewSession(context.Background())
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func execCmd(t *testing.T, s execution.Session, cmd string) execution.Result {
	t.Helper()
	r, err := s.Exec(context.Background(), cmd, execution.ExecOptions{})
	if err != nil {
		t.Fatalf("Exec(%q): %v", cmd, err)
	}
	return r
}

func TestExecRoundTrip(t *testing.T) {
	srv := newServer(t)
	tr := newTransport(t, srv, execssh.Options{Insecure: true})
	s := openSession(t, tr)

	r := execCmd(t, s, "printf 'hello world'")
	if r.Stdout != "hello world" {
		t.Fatalf("stdout = %q", r.Stdout)
	}
	if r.ExitCode != 0 {
		t.Fatalf("exit = %d", r.ExitCode)
	}
	if r.Stderr != "" {
		t.Fatalf("stderr = %q", r.Stderr)
	}

	// The persistent shell survives a second command.
	r2 := execCmd(t, s, "printf 'again'")
	if r2.Stdout != "again" {
		t.Fatalf("second stdout = %q", r2.Stdout)
	}
}

func TestClosingSharedSessionDoesNotCloseOtherSessions(t *testing.T) {
	srv := newServer(t)
	tr := newTransport(t, srv, execssh.Options{Insecure: true, KeepAlive: true})
	first := openSession(t, tr)
	second := openSession(t, tr)

	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	r := execCmd(t, second, "printf 'still-alive'")
	if r.Stdout != "still-alive" {
		t.Fatalf("stdout = %q", r.Stdout)
	}
}

type errorWriter struct{}

func (errorWriter) Write([]byte) (int, error) { return 0, errors.New("writer failed") }

func TestStreamWriterErrorLeavesFramingAligned(t *testing.T) {
	srv := newServer(t)
	tr := newTransport(t, srv, execssh.Options{Insecure: true})
	s := openSession(t, tr)

	_, err := s.Exec(context.Background(), "printf output", execution.ExecOptions{Stream: errorWriter{}})
	if err == nil || !strings.Contains(err.Error(), "writer failed") {
		t.Fatalf("err = %v", err)
	}
	r := execCmd(t, s, "printf aligned")
	if r.Stdout != "aligned" {
		t.Fatalf("stdout = %q", r.Stdout)
	}
}

func TestNonZeroExit(t *testing.T) {
	srv := newServer(t)
	tr := newTransport(t, srv, execssh.Options{Insecure: true})
	s := openSession(t, tr)

	r := execCmd(t, s, "exit 7")
	if r.ExitCode != 7 {
		t.Fatalf("exit = %d, want 7", r.ExitCode)
	}
}

func TestEmptyOutput(t *testing.T) {
	srv := newServer(t)
	tr := newTransport(t, srv, execssh.Options{Insecure: true})
	s := openSession(t, tr)

	r := execCmd(t, s, "true")
	if r.Stdout != "" || r.Stderr != "" || r.ExitCode != 0 {
		t.Fatalf("got %+v", r)
	}
}

func TestStdoutStderrSeparation(t *testing.T) {
	srv := newServer(t)
	tr := newTransport(t, srv, execssh.Options{Insecure: true})
	s := openSession(t, tr)

	r := execCmd(t, s, "printf 'out1\\n'; printf 'err1\\n' >&2; printf 'out2'; printf 'err2' >&2")
	if r.Stdout != "out1\nout2" {
		t.Fatalf("stdout = %q", r.Stdout)
	}
	if r.Stderr != "err1\nerr2" {
		t.Fatalf("stderr = %q", r.Stderr)
	}
}

func TestDelimiterLikeContent(t *testing.T) {
	srv := newServer(t)
	tr := newTransport(t, srv, execssh.Options{Insecure: true})
	s := openSession(t, tr)

	fakeNonce := strings.Repeat("0", 32)
	payload := "pre AIHARN-END-" + fakeNonce + " mid AIHARN-BEGIN-" + fakeNonce + " AIHARN-ERR-" + fakeNonce + " post"

	r := execCmd(t, s, "printf '%s' '"+payload+"'")
	if r.Stdout != payload {
		t.Fatalf("stdout = %q, want payload verbatim", r.Stdout)
	}
	if r.ExitCode != 0 {
		t.Fatalf("exit = %d", r.ExitCode)
	}
}

func TestTruncation(t *testing.T) {
	srv := newServer(t)
	tr := newTransport(t, srv, execssh.Options{Insecure: true})
	s := openSession(t, tr)

	r, err := s.Exec(context.Background(), "yes x | head -c 100000", execution.ExecOptions{MaxOutputBytes: 1000})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if !r.Truncated {
		t.Fatal("expected truncated")
	}
	if len(r.Stdout) != 1000 {
		t.Fatalf("retained stdout = %d bytes, want 1000", len(r.Stdout))
	}
	if r.Stderr != "" {
		t.Fatalf("stderr = %q", r.Stderr)
	}

	// Framing stayed aligned despite dropped bytes.
	r2 := execCmd(t, s, "printf 'after'")
	if r2.Stdout != "after" {
		t.Fatalf("next stdout = %q", r2.Stdout)
	}
}

func TestWorkingDir(t *testing.T) {
	srv := newServer(t)
	tr := newTransport(t, srv, execssh.Options{Insecure: true})
	s := openSession(t, tr)

	dir := t.TempDir()
	r, err := s.Exec(context.Background(), "pwd", execution.ExecOptions{Cwd: dir})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if r.Stdout != dir+"\n" {
		t.Fatalf("stdout = %q, want %q", r.Stdout, dir+"\n")
	}
}

func TestTimeoutLeavesShellUsable(t *testing.T) {
	srv := newServer(t)
	tr := newTransport(t, srv, execssh.Options{Insecure: true})
	s := openSession(t, tr)

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	_, err := s.Exec(ctx, "sleep 30", execution.ExecOptions{})
	if err != nil && errors.Is(err, execution.ErrSessionReset) {
		t.Fatalf("timeout must not reset the session: %v", err)
	}

	r := execCmd(t, s, "printf 'alive'")
	if r.Stdout != "alive" {
		t.Fatalf("post-timeout stdout = %q", r.Stdout)
	}
}

func TestCancelMidStreamThenCleanCommand(t *testing.T) {
	srv := newServer(t)
	tr := newTransport(t, srv, execssh.Options{Insecure: true})
	s := openSession(t, tr)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	var res execution.Result
	var err error
	go func() {
		defer close(done)
		res, err = s.Exec(ctx, "printf 'before-kill'; sleep 30; printf 'after-kill'", execution.ExecOptions{})
	}()

	time.Sleep(300 * time.Millisecond)
	cancel()
	<-done

	if err != nil && errors.Is(err, execution.ErrSessionReset) {
		t.Fatalf("cancel must not reset the session: %v", err)
	}
	if res.Stdout != "before-kill" {
		t.Fatalf("pre-kill stdout = %q", res.Stdout)
	}

	r2 := execCmd(t, s, "printf 'clean'")
	if r2.Stdout != "clean" {
		t.Fatalf("next command leaked prior output: %q", r2.Stdout)
	}
}

func TestReconnectEmitsReset(t *testing.T) {
	srv := newServer(t)
	tr := newTransport(t, srv, execssh.Options{Insecure: true, KeepAlive: false})
	s := openSession(t, tr)

	srv.Close()

	_, err := s.Exec(context.Background(), "printf x", execution.ExecOptions{})
	if !errors.Is(err, execution.ErrSessionReset) {
		t.Fatalf("expected ErrSessionReset, got %v", err)
	}
}

func TestHostKeyAccepted(t *testing.T) {
	srv := newServer(t)
	kh := filepath.Join(t.TempDir(), "known_hosts")
	if err := srv.WriteKnownHosts(kh); err != nil {
		t.Fatalf("WriteKnownHosts: %v", err)
	}
	tr := newTransport(t, srv, execssh.Options{KnownHosts: kh})
	s := openSession(t, tr)

	r := execCmd(t, s, "printf 'ok'")
	if r.Stdout != "ok" {
		t.Fatalf("stdout = %q", r.Stdout)
	}
}

func TestHostKeyRejected(t *testing.T) {
	srv := newServer(t)

	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pub, err := ssh.NewPublicKey(priv.Public())
	if err != nil {
		t.Fatal(err)
	}
	otherKey := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pub)))

	kh := filepath.Join(t.TempDir(), "known_hosts")
	line := fmt.Sprintf("[127.0.0.1]:%d %s\n", srv.Port(), otherKey)
	if err := os.WriteFile(kh, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}

	tr := newTransport(t, srv, execssh.Options{KnownHosts: kh})
	if _, err := tr.NewSession(context.Background()); err == nil {
		t.Fatal("expected host-key error")
	}
}

func TestNewTransportValidation(t *testing.T) {
	cases := []struct {
		name    string
		opts    execssh.Options
		wantErr string
	}{
		{"missing host", execssh.Options{User: "u", Password: "p", Insecure: true}, "host"},
		{"missing user", execssh.Options{Host: "h", Password: "p", Insecure: true}, "user"},
		{"both auth", execssh.Options{Host: "h", User: "u", KeyFile: "k", Password: "p", Insecure: true}, "mutually exclusive"},
		{"missing known_hosts", execssh.Options{Host: "h", User: "u", Password: "p"}, "known_hosts"},
		{"invalid port", execssh.Options{Host: "h", Port: 70000, User: "u", Password: "p", Insecure: true}, "port"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := execssh.NewTransport(tc.opts)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("got %v, want containing %q", err, tc.wantErr)
			}
		})
	}
}
