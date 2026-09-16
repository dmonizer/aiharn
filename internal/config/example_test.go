package config

import (
	"os"
	"path/filepath"
	"testing"
)

// Keep the checked-in example loadable and its advertised default-valued
// sections synchronized with the actual defaulting code.
func TestConfigExampleShowsDefaults(t *testing.T) {
	t.Setenv("DEEPSEEK_API_KEY", "example-test-key")
	t.Setenv("ZAI_API_KEY", "example-test-key")
	path := filepath.Join("..", "..", "config.toml.example")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load example: %v", err)
	}
	if err := Validate(cfg, ValidateOptions{}); err != nil {
		t.Fatalf("validate example: %v", err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AiharnHome != home {
		t.Errorf("aiharn_home = %q, want default %q", cfg.AiharnHome, home)
	}
	if cfg.Shortcuts != DefaultShortcuts() {
		t.Errorf("shortcuts = %+v, want %+v", cfg.Shortcuts, DefaultShortcuts())
	}
	if cfg.Limits != DefaultLimits() {
		t.Errorf("limits = %+v, want %+v", cfg.Limits, DefaultLimits())
	}
	if cfg.Approval.Mode != ApprovalModeAsk {
		t.Errorf("approval.mode = %q, want %q", cfg.Approval.Mode, ApprovalModeAsk)
	}
	if cfg.API.Listen != "" || cfg.API.Token != "" || len(cfg.API.AllowOrigins) != 0 || cfg.API.Only {
		t.Errorf("API should be disabled by default, got %+v", cfg.API)
	}
	ssh := cfg.Channels[0]
	if ssh.Type != ChannelTypeSSH || ssh.Port != 22 || ssh.User != "" ||
		ssh.Auth.KeyFile != "" || ssh.Auth.Password != "" || ssh.KnownHosts != "" ||
		ssh.Insecure || !ssh.KeepAliveEnabled() || ssh.DefaultShell != "/bin/bash" ||
		ssh.RemoteCommand != "" || ssh.WorkingDir.Raw != "" {
		t.Errorf("SSH channel defaults changed: %+v", ssh)
	}
	model := cfg.Models["deepseek_flash"]
	if model.ReasoningEffort != "" || model.ReasoningSummary != "" {
		t.Errorf("reasoning defaults changed: %+v", model)
	}
}
