package local

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"aiharn/internal/execution"
)

func openTestSession(t *testing.T) execution.Session {
	t.Helper()
	tr, err := NewTransport(Options{DefaultShell: "/bin/bash"})
	if err != nil {
		t.Fatalf("NewTransport: %v", err)
	}
	s, err := tr.NewSession(context.Background())
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func execCmd(t *testing.T, s execution.Session, cmd string) execution.Result {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	r, err := s.Exec(ctx, cmd, execution.ExecOptions{MaxOutputBytes: 1 << 20})
	if err != nil {
		t.Fatalf("Exec(%q): %v", cmd, err)
	}
	return r
}

func TestExecStdout(t *testing.T) {
	s := openTestSession(t)
	r := execCmd(t, s, `printf 'hello-from-shell'`)
	if r.ExitCode != 0 {
		t.Fatalf("exit = %d, stderr = %q", r.ExitCode, r.Stderr)
	}
	if r.Stdout != "hello-from-shell" {
		t.Fatalf("stdout = %q", r.Stdout)
	}
}

func TestExecStderrAndExitCode(t *testing.T) {
	s := openTestSession(t)
	ctx := context.Background()
	r, err := s.Exec(ctx, `echo boom >&2; exit 7`, execution.ExecOptions{MaxOutputBytes: 1 << 20})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if r.ExitCode != 7 {
		t.Fatalf("exit = %d, want 7", r.ExitCode)
	}
	if strings.TrimSpace(r.Stderr) != "boom" {
		t.Fatalf("stderr = %q", r.Stderr)
	}
}

func TestExecCwd(t *testing.T) {
	s := openTestSession(t)
	dir := t.TempDir()
	ctx := context.Background()
	r, err := s.Exec(ctx, `pwd`, execution.ExecOptions{Cwd: dir, MaxOutputBytes: 1 << 20})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if strings.TrimSpace(r.Stdout) != dir {
		t.Fatalf("pwd = %q, want %q", r.Stdout, dir)
	}
}

func TestExecHome(t *testing.T) {
	s := openTestSession(t)
	r := execCmd(t, s, `printf '%s' "$HOME"`)
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Clean(r.Stdout) != filepath.Clean(home) {
		t.Fatalf("$HOME = %q, want %q", r.Stdout, home)
	}
}

func TestExecTruncation(t *testing.T) {
	s := openTestSession(t)
	ctx := context.Background()
	r, err := s.Exec(ctx, `printf '0123456789'`, execution.ExecOptions{MaxOutputBytes: 4})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if !r.Truncated {
		t.Fatal("expected truncation")
	}
	if r.Stdout != "0123" {
		t.Fatalf("stdout = %q, want %q", r.Stdout, "0123")
	}
}

func TestTimeoutLeavesShellUsable(t *testing.T) {
	s := openTestSession(t)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, err := s.Exec(ctx, `sleep 30`, execution.ExecOptions{MaxOutputBytes: 1 << 20})
	if err != nil && errors.Is(err, execution.ErrSessionReset) {
		t.Fatalf("timeout must not reset the session: %v", err)
	}

	r := execCmd(t, s, `printf 'alive'`)
	if r.Stdout != "alive" {
		t.Fatalf("post-timeout stdout = %q", r.Stdout)
	}
}
