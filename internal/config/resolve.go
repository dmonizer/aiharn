package config

import (
	"os"
	"path/filepath"
	"strings"
)

// expandTilde expands a leading "~" for LOCAL paths (SSH key files, known_hosts,
// system prompts). Remote working-directory templates must not pass through here.
func expandTilde(p string) string {
	if p == "" {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return p // leave as-is; validation/use will surface a clearer error later
	}
	if p == "~" {
		return home
	}
	if strings.HasPrefix(p, "~/") {
		return filepath.Join(home, p[2:])
	}
	return p
}

// resolveLocalPath makes p absolute relative to baseDir (the config file's
// directory), expanding a leading "~" first. Empty strings pass through.
func resolveLocalPath(baseDir, p string) string {
	if p == "" {
		return p
	}
	p = expandTilde(p)
	if filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	return filepath.Join(baseDir, p)
}

// resolvePaths rewrites local file paths in place so they are absolute and
// independent of the process working directory.
func resolvePaths(cfg *Config, baseDir string) {
	cfg.AiharnHome = resolveLocalPath(baseDir, cfg.AiharnHome)
	cfg.API.AuthFile = resolveLocalPath(baseDir, cfg.API.AuthFile)
	for i := range cfg.Channels {
		c := &cfg.Channels[i]
		c.Auth.KeyFile = resolveLocalPath(baseDir, c.Auth.KeyFile)
		c.KnownHosts = resolveLocalPath(baseDir, c.KnownHosts)
	}
	for name, a := range cfg.Agents {
		a.SystemPrompt = resolveLocalPath(baseDir, a.SystemPrompt)
		cfg.Agents[name] = a
	}
}
