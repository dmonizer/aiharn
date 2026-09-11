package ssh

import (
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/kevinburke/ssh_config"
)

// defaultSSHConfigPath returns the path to the user's OpenSSH config.
func defaultSSHConfigPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("ssh: resolve home dir: %w", err)
	}
	return filepath.Join(home, ".ssh", "config"), nil
}

// resolveAlias resolves opts.Host as an OpenSSH config alias read from
// configPath, filling in the host name, port, user, identity file, and
// known_hosts. It leaves non-connection options (KeepAlive, DefaultShell)
// untouched, and honors an explicit port only when the config does not set one.
func resolveAlias(opts Options, configPath string) (Options, error) {
	f, err := os.Open(configPath)
	if err != nil {
		return opts, fmt.Errorf("ssh: open %s: %w", configPath, err)
	}
	defer f.Close()

	cfg, err := ssh_config.Decode(f)
	if err != nil {
		return opts, fmt.Errorf("ssh: parse %s: %w", configPath, err)
	}
	alias := opts.Host

	opts.Host = get(cfg, alias, "HostName")
	if opts.Host == "" {
		opts.Host = alias
	}

	if portStr := get(cfg, alias, "Port"); portStr != "" {
		port, err := strconv.Atoi(portStr)
		if err != nil {
			return opts, fmt.Errorf("ssh: invalid Port %q for alias %q in %s", portStr, alias, configPath)
		}
		opts.Port = port
	}

	opts.User = get(cfg, alias, "User")
	if opts.User == "" {
		opts.User = currentUser()
		if opts.User == "" {
			return opts, fmt.Errorf("ssh: alias %q has no User in %s and no local user found", alias, configPath)
		}
	}

	identityFile := get(cfg, alias, "IdentityFile")
	if identityFile == "" {
		return opts, fmt.Errorf("ssh: alias %q has no IdentityFile in %s", alias, configPath)
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return opts, fmt.Errorf("ssh: resolve home dir: %w", err)
	}
	sshDir := filepath.Join(home, ".ssh")

	opts.KeyFile = resolveIdentityPath(identityFile, sshDir)
	if opts.KnownHosts == "" {
		opts.KnownHosts = filepath.Join(sshDir, "known_hosts")
	}
	return opts, nil
}

// get returns the first config value for key that applies to alias, or "".
func get(cfg *ssh_config.Config, alias, key string) string {
	v, _ := cfg.Get(alias, key)
	return v
}

// currentUser returns the local username, falling back to $USER.
func currentUser() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	return os.Getenv("USER")
}

// resolveIdentityPath expands a leading "~" and resolves bare relative paths
// against sshDir (matching OpenSSH's convention for IdentityFile).
func resolveIdentityPath(s, sshDir string) string {
	if s == "~" {
		return filepath.Dir(sshDir)
	}
	if strings.HasPrefix(s, "~/") {
		return filepath.Join(filepath.Dir(sshDir), s[2:])
	}
	if filepath.IsAbs(s) {
		return s
	}
	return filepath.Join(sshDir, s)
}
