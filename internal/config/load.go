package config

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

// DefaultAiharnHome returns the local directory used for Aiharn's config,
// skills, prompts, and transcripts when aiharn_home is not configured.
func DefaultAiharnHome() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".aiharn"), nil
}

// DefaultConfigPath returns the configuration file used when --config is not
// supplied.
func DefaultConfigPath() (string, error) {
	home, err := DefaultAiharnHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "config.toml"), nil
}

// Load reads, parses, interpolates, resolves, and defaults the configuration at
// path. It performs the mechanical steps only; call Validate for semantic
// checks. Secret-bearing fields are never embedded in returned errors.
func Load(path string) (*Config, error) {
	const maxConfigBytes = 4 << 20
	data, err := readLimitedFile(path, maxConfigBytes)
	if err != nil {
		return nil, fmt.Errorf("config: read %s: %w", path, err)
	}

	var cfg Config
	md, err := toml.Decode(string(data), &cfg)
	if err != nil {
		return nil, fmt.Errorf("config: parse: %w", err)
	}
	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		return nil, fmt.Errorf("config: unknown field(s): %s", formatKeys(undecoded))
	}

	if err := interpolate(&cfg); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}

	baseDir, err := filepath.Abs(filepath.Dir(path))
	if err != nil {
		return nil, fmt.Errorf("config: resolve base dir: %w", err)
	}
	if cfg.AiharnHome == "" {
		home, err := DefaultAiharnHome()
		if err != nil {
			return nil, fmt.Errorf("config: resolve user home directory for aiharn_home: %w", err)
		}
		cfg.AiharnHome = home
	}
	resolvePaths(&cfg, baseDir)
	applyDefaults(&cfg)

	return &cfg, nil
}

func readLimitedFile(path string, maxBytes int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > maxBytes {
		return nil, fmt.Errorf("file exceeds %d-byte limit", maxBytes)
	}
	return b, nil
}

// applyDefaults fills zero-valued fields with their defaults. Zero = "use the
// default" for every defaultable field; the only field needing pointer
// semantics is ChannelConfig.KeepAlive, whose meaningful explicit value (false)
// differs from its default (true).
func applyDefaults(cfg *Config) {
	if cfg.Models == nil {
		cfg.Models = map[string]ModelConfig{}
	}
	if cfg.Agents == nil {
		cfg.Agents = map[string]AgentConfig{}
	}
	for i := range cfg.Channels {
		c := &cfg.Channels[i]
		if c.Type == "" {
			c.Type = defaultChannelType
		}
		if c.Port == 0 {
			c.Port = defaultSSHPort
		}
		if c.KeepAlive == nil {
			k := true
			c.KeepAlive = &k
		}
		if c.DefaultShell == "" {
			c.DefaultShell = defaultShell
		}
	}

	for name, a := range cfg.Agents {
		if a.Tools.Mode == "" {
			a.Tools.Mode = defaultToolsMode
		}
		cfg.Agents[name] = a
	}

	d := DefaultLimits()
	if cfg.Limits.MaxAgentDepth == 0 {
		cfg.Limits.MaxAgentDepth = d.MaxAgentDepth
	}
	if cfg.Limits.MaxOpenAgents == 0 {
		cfg.Limits.MaxOpenAgents = d.MaxOpenAgents
	}
	if cfg.Limits.CommandTimeout == 0 {
		cfg.Limits.CommandTimeout = d.CommandTimeout
	}
	if cfg.Limits.CommandOutputBytes == 0 {
		cfg.Limits.CommandOutputBytes = d.CommandOutputBytes
	}
	if cfg.Limits.ToolResultBytes == 0 {
		cfg.Limits.ToolResultBytes = d.ToolResultBytes
	}
	if cfg.Limits.InboxDepth == 0 {
		cfg.Limits.InboxDepth = d.InboxDepth
	}
	if cfg.Limits.EventCapacity == 0 {
		cfg.Limits.EventCapacity = d.EventCapacity
	}
	if cfg.Limits.ToolcallsPerTurn == 0 {
		cfg.Limits.ToolcallsPerTurn = d.ToolcallsPerTurn
	}
	if cfg.Limits.ThinkingTimeout == 0 {
		cfg.Limits.ThinkingTimeout = d.ThinkingTimeout
	}
	if cfg.Limits.RequestTimeout == 0 {
		cfg.Limits.RequestTimeout = d.RequestTimeout
	}
	if cfg.Limits.TranscriptMaxItems == 0 {
		cfg.Limits.TranscriptMaxItems = d.TranscriptMaxItems
	}
	if cfg.Limits.TranscriptMaxBytes == 0 {
		cfg.Limits.TranscriptMaxBytes = d.TranscriptMaxBytes
	}

	if cfg.Approval.Mode == "" {
		cfg.Approval.Mode = defaultApprovalMode
	}
	cfg.Shortcuts = cfg.Shortcuts.WithDefaults()
}

// formatKeys renders []toml.Key as "a.b.c" for error messages.
func formatKeys(keys []toml.Key) string {
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, strings.Join(k, "."))
	}
	return strings.Join(parts, ", ")
}
