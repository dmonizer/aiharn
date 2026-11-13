package app

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"aiharn/internal/config"
	"aiharn/internal/execution"
	execlocal "aiharn/internal/execution/local"
)

func TestBoundedDirectory(t *testing.T) {
	for _, dir := range []string{"../..", "/srv/root-other", "../%2e%2e", "%252e%252e", "..\\..", "/tmp", ""} {
		if _, err := boundedDirectory("/srv/root", "/srv/root/sub", dir); err == nil {
			t.Errorf("accepted %q", dir)
		}
	}
	for _, dir := range []string{"..", "a/../b", "/srv/root/sub"} {
		if _, err := boundedDirectory("/srv/root", "/srv/root/sub", dir); err != nil {
			t.Errorf("rejected %q: %v", dir, err)
		}
	}
}

func TestDirectorySettingLocal(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	sub := filepath.Join(root, "sub")
	if err := os.Mkdir(sub, 0700); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	transport, err := execlocal.NewTransport(execlocal.Options{DefaultShell: "/bin/bash"})
	if err != nil {
		t.Fatal(err)
	}
	defer transport.Close()
	shell, err := transport.NewSession(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer shell.Close()
	c := &channelController{cfg: &config.Config{Channels: []config.ChannelConfig{{Name: "local", Root: root}}}, name: "local", cwd: root, session: shell}
	s := &Session{rt: &Runtime{channel: c}}
	if err := s.SetWorkingDirectory(ctx, "sub"); err != nil {
		t.Fatal(err)
	}
	if s.WorkingDirectory() != sub {
		t.Fatal(s.WorkingDirectory())
	}
	for _, dir := range []string{"../..", "../escape", "%2e%2e"} {
		if err := s.SetWorkingDirectory(ctx, dir); err == nil {
			t.Errorf("accepted %q", dir)
		}
		if s.WorkingDirectory() != sub {
			t.Fatal("failed change mutated cwd")
		}
	}
	r, err := c.Exec(ctx, "pwd", execution.ExecOptions{})
	if err != nil || r.Stdout != sub+"\n" {
		t.Fatalf("execution cwd: %+v %v", r, err)
	}
}
