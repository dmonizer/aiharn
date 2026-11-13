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

func TestDirectoriesStayWithChannel(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	a := filepath.Join(root, "a")
	b := filepath.Join(root, "b")
	for _, dir := range []string{a, b} {
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	cfg := &config.Config{Channels: []config.ChannelConfig{
		{Name: "one", Type: "local", Root: root, DefaultShell: "/bin/bash"},
		{Name: "two", Type: "local", Root: root, DefaultShell: "/bin/bash"},
	}}
	tc := &transportCache{transports: map[string]execution.Transport{}}
	defer tc.closeAll()
	c, err := newChannelController(ctx, cfg, tc, config.AgentConfig{}, "main", "one")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	s := &Session{rt: &Runtime{channel: c}}
	if err := s.SetWorkingDirectory(ctx, a); err != nil {
		t.Fatal(err)
	}
	if value, err := s.ChannelWorkingDirectory(ctx, "two", &b); err != nil || value != b {
		t.Fatalf("directory two: %q %v", value, err)
	}
	if c.Name() != "one" || c.Cwd() != a {
		t.Fatal("inactive edit changed active channel")
	}
	if err := c.Set(ctx, "two"); err != nil {
		t.Fatal(err)
	}
	if c.Cwd() != b {
		t.Fatal("second directory lost")
	}
	if err := c.Set(ctx, "one"); err != nil {
		t.Fatal(err)
	}
	if c.Cwd() != a {
		t.Fatal("first directory lost")
	}
}
