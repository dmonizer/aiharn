package ssh

import (
	"os"
	"path/filepath"
	"reflect"
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
	home := t.TempDir()
	t.Setenv("HOME", home)
	sshDir := filepath.Join(home, ".ssh")

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

	if opts.KeyFile != filepath.Join(sshDir, "id_ed25519") {
		t.Fatalf("KeyFile = %q", opts.KeyFile)
	}
	if opts.KnownHosts != filepath.Join(sshDir, "known_hosts") {
		t.Fatalf("KnownHosts = %q", opts.KnownHosts)
	}
}

func TestResolveAliasDefaults(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

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
	t.Setenv("HOME", t.TempDir())

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

func TestResolveAliasSetEnv(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	config := `Host myalias
	HostName 10.0.0.5
	SetEnv SECRET=abc123
	SetEnv TOKEN=def456
	SetEnv EMPTY=
	SetEnv noequals
`
	opts, err := resolveAlias(Options{Host: "myalias"}, writeSSHConfig(t, config))
	if err != nil {
		t.Fatal(err)
	}

	want := []string{"SECRET=abc123", "TOKEN=def456", "EMPTY="}
	if len(opts.Env) != len(want) {
		t.Fatalf("Env = %#v, want %#v", opts.Env, want)
	}
	for i := range want {
		if opts.Env[i] != want[i] {
			t.Fatalf("Env[%d] = %q, want %q", i, opts.Env[i], want[i])
		}
	}
}

func TestResolveAliasMissingConfig(t *testing.T) {
	_, err := resolveAlias(Options{Host: "myalias"}, filepath.Join(t.TempDir(), "nope"))
	if err == nil || !strings.Contains(err.Error(), "open") {
		t.Fatalf("err = %v, want open error", err)
	}
}

func TestResolveAliasHonorsUserKnownHostsFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	config := `Host myalias
	HostName 10.0.0.5
	User ubuntu
	UserKnownHostsFile ~/.ssh/known_hosts_custom ignored_token
`
	opts, err := resolveAlias(Options{Host: "myalias"}, writeSSHConfig(t, config))
	if err != nil {
		t.Fatal(err)
	}

	want := filepath.Join(home, ".ssh", "known_hosts_custom")
	if opts.KnownHosts != want {
		t.Fatalf("KnownHosts = %q, want %q", opts.KnownHosts, want)
	}
}

func TestResolveAliasPreservesExplicitKeyAndKnownHosts(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	config := `Host myalias
	HostName 10.0.0.5
	User aliasuser
`
	opts, err := resolveAlias(Options{
		Host:       "myalias",
		KeyFile:    "/explicit/key",
		KnownHosts: "/explicit/known_hosts",
	}, writeSSHConfig(t, config))
	if err != nil {
		t.Fatal(err)
	}

	if opts.Host != "10.0.0.5" {
		t.Fatalf("Host = %q", opts.Host)
	}
	if opts.User != "aliasuser" {
		t.Fatalf("User = %q", opts.User)
	}
	if opts.KeyFile != "/explicit/key" {
		t.Fatalf("KeyFile = %q, want explicit key preserved", opts.KeyFile)
	}
	if opts.KnownHosts != "/explicit/known_hosts" {
		t.Fatalf("KnownHosts = %q, want explicit known_hosts preserved", opts.KnownHosts)
	}
}

func TestTryResolveAliasFound(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	sshDir := filepath.Join(home, ".ssh")

	config := `Host kali
	HostName 10.0.0.9
	User kali
	IdentityFile ~/.ssh/id_kali
	UserKnownHostsFile ~/.ssh/known_hosts_kali
`
	resolved, found, err := tryResolveAlias(Options{Host: "kali"}, writeSSHConfig(t, config))
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("found = false, want true")
	}
	if resolved.Host != "10.0.0.9" {
		t.Fatalf("Host = %q", resolved.Host)
	}
	if resolved.User != "kali" {
		t.Fatalf("User = %q", resolved.User)
	}
	if resolved.KeyFile != filepath.Join(sshDir, "id_kali") {
		t.Fatalf("KeyFile = %q", resolved.KeyFile)
	}
	if resolved.KnownHosts != filepath.Join(sshDir, "known_hosts_kali") {
		t.Fatalf("KnownHosts = %q", resolved.KnownHosts)
	}
}

func TestTryResolveAliasNoMatch(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	opts := Options{
		Host:       "other",
		User:       "explicituser",
		KeyFile:    "/explicit/key",
		KnownHosts: "/explicit/known_hosts",
	}
	resolved, found, err := tryResolveAlias(opts, writeSSHConfig(t, "Host kali\n\tHostName 10.0.0.9\n"))
	if err != nil {
		t.Fatal(err)
	}
	if found {
		t.Fatal("found = true, want false")
	}
	if !reflect.DeepEqual(resolved, opts) {
		t.Fatalf("resolved = %#v, want unchanged %#v", resolved, opts)
	}
}

func TestTryResolveAliasMissingConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	opts := Options{
		Host:       "kali",
		User:       "explicituser",
		KeyFile:    "/explicit/key",
		KnownHosts: "/explicit/known_hosts",
	}
	resolved, found, err := tryResolveAlias(opts, filepath.Join(home, ".ssh", "config"))
	if err != nil {
		t.Fatalf("err = %v, want nil for missing config", err)
	}
	if found {
		t.Fatal("found = true, want false")
	}
	if !reflect.DeepEqual(resolved, opts) {
		t.Fatalf("resolved = %#v, want unchanged %#v", resolved, opts)
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
	if err := os.WriteFile(filepath.Join(sshDir, "id_ed25519"), []byte("test key"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sshDir, "known_hosts"), nil, 0o600); err != nil {
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
	if tr.opts.KeyFile != filepath.Join(sshDir, "id_ed25519") {
		t.Fatalf("resolved KeyFile = %q", tr.opts.KeyFile)
	}
	if tr.opts.KnownHosts != filepath.Join(sshDir, "known_hosts") {
		t.Fatalf("resolved KnownHosts = %q", tr.opts.KnownHosts)
	}
}

func TestNewTransportPrefersAliasWithExplicitOpts(t *testing.T) {
	home := t.TempDir()
	sshDir := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(sshDir, 0o700); err != nil {
		t.Fatal(err)
	}
	config := "Host kali\n\tHostName 10.0.0.9\n\tPort 2222\n\tUser kali\n\tIdentityFile ~/.ssh/id_kali\n\tUserKnownHostsFile ~/.ssh/known_hosts_kali\n"
	if err := os.WriteFile(filepath.Join(sshDir, "config"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sshDir, "id_kali"), []byte("test key"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sshDir, "known_hosts_kali"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)

	tr, err := NewTransport(Options{
		Host:           "kali",
		Port:           22,
		User:           "explicituser",
		KeyFile:        "/explicit/key",
		KnownHosts:     "/explicit/known_hosts",
		SSHConfigAlias: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	if tr.opts.Host != "10.0.0.9" {
		t.Fatalf("resolved HostName = %q", tr.opts.Host)
	}
	if tr.opts.Port != 2222 {
		t.Fatalf("resolved Port = %d", tr.opts.Port)
	}
	if tr.opts.User != "kali" {
		t.Fatalf("resolved User = %q", tr.opts.User)
	}
	if tr.opts.KeyFile != filepath.Join(sshDir, "id_kali") {
		t.Fatalf("resolved KeyFile = %q", tr.opts.KeyFile)
	}
	if tr.opts.KnownHosts != filepath.Join(sshDir, "known_hosts_kali") {
		t.Fatalf("resolved KnownHosts = %q", tr.opts.KnownHosts)
	}
}

func TestNewTransportFallsBackToExplicitOptsWhenNoAlias(t *testing.T) {
	home := t.TempDir()
	sshDir := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(sshDir, 0o700); err != nil {
		t.Fatal(err)
	}
	config := "Host other\n\tHostName 10.0.0.9\n"
	if err := os.WriteFile(filepath.Join(sshDir, "config"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(sshDir, "explicit_key")
	khPath := filepath.Join(sshDir, "explicit_known_hosts")
	if err := os.WriteFile(keyPath, []byte("test key"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(khPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)

	tr, err := NewTransport(Options{
		Host:           "myhost",
		Port:           22,
		User:           "explicituser",
		KeyFile:        keyPath,
		KnownHosts:     khPath,
		SSHConfigAlias: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	if tr.opts.Host != "myhost" {
		t.Fatalf("resolved HostName = %q", tr.opts.Host)
	}
	if tr.opts.User != "explicituser" {
		t.Fatalf("resolved User = %q", tr.opts.User)
	}
	if tr.opts.KeyFile != keyPath {
		t.Fatalf("resolved KeyFile = %q", tr.opts.KeyFile)
	}
	if tr.opts.KnownHosts != khPath {
		t.Fatalf("resolved KnownHosts = %q", tr.opts.KnownHosts)
	}
}

func TestNewTransportIPHostSkipsAlias(t *testing.T) {
	home := t.TempDir()
	sshDir := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(sshDir, 0o700); err != nil {
		t.Fatal(err)
	}
	config := "Host 10.0.0.5\n\tHostName 10.0.0.99\n\tUser aliasuser\n"
	if err := os.WriteFile(filepath.Join(sshDir, "config"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(sshDir, "explicit_key")
	khPath := filepath.Join(sshDir, "explicit_known_hosts")
	if err := os.WriteFile(keyPath, []byte("test key"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(khPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)

	tr, err := NewTransport(Options{
		Host:           "10.0.0.5",
		Port:           22,
		User:           "explicituser",
		KeyFile:        keyPath,
		KnownHosts:     khPath,
		SSHConfigAlias: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	if tr.opts.Host != "10.0.0.5" {
		t.Fatalf("resolved HostName = %q, want IP literal preserved", tr.opts.Host)
	}
	if tr.opts.User != "explicituser" {
		t.Fatalf("resolved User = %q", tr.opts.User)
	}
}
