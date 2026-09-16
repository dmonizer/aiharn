package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestValidateLocalChannel ensures a local channel validates with no host, user,
// auth, or known_hosts — the fields SSH requires.
func TestValidateLocalChannel(t *testing.T) {
	dir := t.TempDir()
	promptPath := filepath.Join(dir, "prompt.md")
	if err := os.WriteFile(promptPath, []byte("hi"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := &Config{
		AiharnHome: dir,
		Models: map[string]ModelConfig{
			"m": {Provider: ProviderOpenAIResponses, BaseURL: "https://api.example.com", APIKey: "k", Model: "gpt"},
		},
		Channels: []ChannelConfig{
			{Name: "local", Type: ChannelTypeLocal, DefaultShell: "/bin/bash"},
		},
		Agents: map[string]AgentConfig{
			"main": {Model: "m", SystemPrompt: promptPath, Channel: "local", Tools: ToolSelection{Mode: ToolModeNone}},
		},
		Limits:   DefaultLimits(),
		Approval: ApprovalConfig{Mode: ApprovalModeAsk},
	}

	if err := Validate(cfg, ValidateOptions{}); err != nil {
		t.Fatalf("local channel should validate: %v", err)
	}
}
