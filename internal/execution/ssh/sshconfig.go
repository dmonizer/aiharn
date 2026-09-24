package ssh

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/kevinburke/ssh_config"

	"aiharn/internal/logging"
)

// defaultSSHConfigPath returns the path to the user's OpenSSH config.
func defaultSSHConfigPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("ssh: resolve home dir: %w", err)
	}
	return filepath.Join(home, ".ssh", "config"), nil
}

// loadSSHConfig opens, reads, and parses the OpenSSH config at configPath.
func loadSSHConfig(configPath string) (*ssh_config.Config, error) {
	f, err := os.Open(configPath)
	if err != nil {
		return nil, fmt.Errorf("ssh: open %s: %w", configPath, err)
	}
	defer f.Close()

	const maxSSHConfigBytes = 4 << 20
	data, err := io.ReadAll(io.LimitReader(f, maxSSHConfigBytes+1))
	if err != nil {
		return nil, fmt.Errorf("ssh: read %s: %w", configPath, err)
	}
	if len(data) > maxSSHConfigBytes {
		return nil, fmt.Errorf("ssh: config %s exceeds %d-byte limit", configPath, maxSSHConfigBytes)
	}
	cfg, err := ssh_config.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("ssh: parse %s: %w", configPath, err)
	}
	return cfg, nil
}

// resolveAlias resolves opts.Host as an OpenSSH config alias read from
// configPath, filling in the host name, port, user, identity file, and
// known_hosts. IdentityFile is optional: when the alias omits it, KeyFile stays
// empty and authentication falls back to the SSH agent. It leaves non-connection
// options (KeepAlive, DefaultShell) untouched, and honors an explicit port only
// when the config does not set one.
func resolveAlias(opts Options, configPath string) (Options, error) {
	cfg, err := loadSSHConfig(configPath)
	if err != nil {
		return opts, err
	}
	return applyAlias(cfg, opts, configPath)
}

// applyAlias applies the SSH config alias matching opts.Host to opts. Config
// values win over explicit opts only for the fields the alias actually sets.
func applyAlias(cfg *ssh_config.Config, opts Options, configPath string) (Options, error) {
	alias := opts.Host

	if hostName := get(cfg, alias, "HostName"); hostName != "" {
		opts.Host = hostName
	}

	if portStr := get(cfg, alias, "Port"); portStr != "" {
		port, err := strconv.Atoi(portStr)
		if err != nil {
			return opts, fmt.Errorf("ssh: invalid Port %q for alias %q in %s", portStr, alias, configPath)
		}
		opts.Port = port
	}

	if user := get(cfg, alias, "User"); user != "" {
		opts.User = user
	}
	if opts.User == "" {
		opts.User = currentUser()
		if opts.User == "" {
			return opts, fmt.Errorf("ssh: alias %q has no User in %s and no local user found", alias, configPath)
		}
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return opts, fmt.Errorf("ssh: resolve home dir: %w", err)
	}
	sshDir := filepath.Join(home, ".ssh")

	if identityFile := get(cfg, alias, "IdentityFile"); identityFile != "" {
		opts.KeyFile = resolveIdentityPath(identityFile, sshDir)
	}

	if knownHostsFile := get(cfg, alias, "UserKnownHostsFile"); knownHostsFile != "" {
		if fields := strings.Fields(knownHostsFile); len(fields) > 0 {
			opts.KnownHosts = resolveKnownHostsPath(fields[0], sshDir)
		}
	}
	if opts.KnownHosts == "" {
		opts.KnownHosts = filepath.Join(sshDir, "known_hosts")
	}

	opts.Env = append(opts.Env, getSetEnv(cfg, alias)...)
	logging.Debug("ssh: resolveAlias",
		slog.String("component", "ssh"),
		slog.String("alias", alias),
		slog.String("host", opts.Host),
		slog.Int("port", opts.Port),
		slog.String("user", opts.User),
		slog.Bool("has_identity_file", opts.KeyFile != ""),
	)
	return opts, nil
}

// tryResolveAlias resolves opts.Host as an OpenSSH config alias when one
// exists. It returns (opts, false, nil) when the config file or the alias is
// absent, leaving opts unchanged. A matching alias is applied and returned with
// found=true. Load/parse errors are returned as-is.
func tryResolveAlias(opts Options, configPath string) (Options, bool, error) {
	cfg, err := loadSSHConfig(configPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return opts, false, nil
		}
		return opts, false, err
	}
	if !hasAlias(cfg, opts.Host) {
		return opts, false, nil
	}
	resolved, err := applyAlias(cfg, opts, configPath)
	if err != nil {
		return opts, false, err
	}
	return resolved, true, nil
}

// hasAlias reports whether the config has a non-empty Host declaration matching
// alias (a Host with at least one key/value node), avoiding the parser's
// implicit empty "Host *".
func hasAlias(cfg *ssh_config.Config, alias string) bool {
	if cfg == nil {
		return false
	}
	for _, host := range cfg.Hosts {
		if !host.Matches(alias) {
			continue
		}
		for _, node := range host.Nodes {
			if _, ok := node.(*ssh_config.KV); ok {
				return true
			}
		}
	}
	return false
}

// isIPLiteral reports whether host is an IP literal, optionally wrapped in the
// bracketed form used by SSH for IPv6 addresses.
func isIPLiteral(host string) bool {
	if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
		host = host[1 : len(host)-1]
	}
	return net.ParseIP(host) != nil
}

// aliasOnly reports whether opts carries only a name and host, i.e. the
// historical pure ~/.ssh/config alias mode.
func aliasOnly(opts Options) bool {
	return opts.User == "" && opts.KeyFile == "" && opts.Password == "" &&
		opts.KnownHosts == "" && !opts.Insecure
}

// get returns the first config value for key that applies to alias, or "".
func get(cfg *ssh_config.Config, alias, key string) string {
	v, _ := cfg.Get(alias, key)
	return v
}

// getSetEnv returns the well-formed SetEnv directives applying to alias, each as
// a "NAME=value" string. Malformed entries (missing "=" or empty name) are
// dropped rather than failing startup.
func getSetEnv(cfg *ssh_config.Config, alias string) []string {
	values, err := cfg.GetAll(alias, "SetEnv")
	if err != nil {
		return nil
	}
	var envs []string
	for _, v := range values {
		v = strings.TrimSpace(v)
		name, _, ok := strings.Cut(v, "=")
		if !ok || name == "" {
			continue
		}
		envs = append(envs, v)
	}
	return envs
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

// resolveKnownHostsPath expands a leading "~" and resolves bare relative paths
// against sshDir, matching resolveIdentityPath for UserKnownHostsFile.
func resolveKnownHostsPath(s, sshDir string) string {
	return resolveIdentityPath(s, sshDir)
}
