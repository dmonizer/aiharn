package ssh

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeSSHConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestResolveAlias(t *testing.T) {
	config := `Host myalias
	HostName 10.0.0.5
	Port 2222
	User ubuntu
	IdentityFile ~/.ssh/id_ed25519
`
	opts, err := resolveAlias(Options{Host: "myalias"}, writeSSHConfig(t, config))
	if err != nil {
		t.Fatal(err)
	}

	if opts.Host != "10.0.0.5" {
		t.Fatalf("HostName = %q", opts.Host)
	}
	if opts.Port != 2222 {
		t.Fatalf("Port = %d", opts.Port)
	}
	if opts.User != "ubuntu" {
		t.Fatalf("User = %q", opts.User)
	}

	home, _ := os.UserHomeDir()
	if opts.KeyFile != filepath.Join(home, ".ssh", "id_ed25519") {
		t.Fatalf("KeyFile = %q", opts.KeyFile)
	}
	if opts.KnownHosts != filepath.Join(home, ".ssh", "known_hosts") {
		t.Fatalf("KnownHosts = %q", opts.KnownHosts)
	}
}

func TestResolveAliasDefaults(t *testing.T) {
	config := `Host myalias
	IdentityFile ~/.ssh/id_ed25519
`
	opts, err := resolveAlias(Options{Host: "myalias", Port: 22}, writeSSHConfig(t, config))
	if err != nil {
		t.Fatal(err)
	}

	if opts.Host != "myalias" {
		t.Fatalf("Host = %q, want the alias itself", opts.Host)
	}
	if opts.Port != 22 {
		t.Fatalf("Port = %d, want the explicit port kept", opts.Port)
	}
	if opts.User == "" {
		t.Fatal("User should default to the local user")
	}
}

func TestResolveAliasWithoutIdentityFile(t *testing.T) {
	// An alias with no IdentityFile leaves KeyFile empty so auth falls back to
	// the SSH agent; it is not an error.
	opts, err := resolveAlias(Options{Host: "myalias"}, writeSSHConfig(t, "Host myalias\n\tHostName 10.0.0.5\n"))
	if err != nil {
		t.Fatal(err)
	}
	if opts.Host != "10.0.0.5" {
		t.Fatalf("Host = %q", opts.Host)
	}
	if opts.KeyFile != "" {
		t.Fatalf("KeyFile = %q, want empty (agent fallback)", opts.KeyFile)
	}
	if opts.KnownHosts == "" {
		t.Fatal("KnownHosts should still default")
	}
}

func TestResolveAliasMissingConfig(t *testing.T) {
	_, err := resolveAlias(Options{Host: "myalias"}, filepath.Join(t.TempDir(), "nope"))
	if err == nil || !strings.Contains(err.Error(), "open") {
		t.Fatalf("err = %v, want open error", err)
	}
}

func TestNewTransportResolvesAlias(t *testing.T) {
	home := t.TempDir()
	sshDir := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(sshDir, 0o700); err != nil {
		t.Fatal(err)
	}
	config := "Host foo\n\tHostName 10.0.0.5\n\tPort 2222\n\tUser ubuntu\n\tIdentityFile ~/.ssh/id_ed25519\n"
	if err := os.WriteFile(filepath.Join(sshDir, "config"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)

	tr, err := NewTransport(Options{Host: "foo", SSHConfigAlias: true})
	if err != nil {
		t.Fatal(err)
	}

	if tr.opts.Host != "10.0.0.5" {
		t.Fatalf("resolved HostName = %q", tr.opts.Host)
	}
	if tr.opts.Port != 2222 {
		t.Fatalf("resolved Port = %d", tr.opts.Port)
	}
	if tr.opts.User != "ubuntu" {
		t.Fatalf("resolved User = %q", tr.opts.User)
	}
	if tr.opts.KeyFile != filepath.Join(home, ".ssh", "id_ed25519") {
		t.Fatalf("resolved KeyFile = %q", tr.opts.KeyFile)
	}
	if tr.opts.KnownHosts != filepath.Join(sshDir, "known_hosts") {
		t.Fatalf("resolved KnownHosts = %q", tr.opts.KnownHosts)
	}
}
