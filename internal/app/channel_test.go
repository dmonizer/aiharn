package app_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"aiharn/internal/app"
	"aiharn/internal/config"
	"aiharn/internal/execution/ssh/harness"
)

// TestSubagentInheritsMainChannel proves that a subagent type with no explicit
// channel runs on the main agent's channel rather than the configured default
// (the first channel). The main agent uses the second channel (an SSH harness);
// the first channel is local. After the spawn, the harness must have served two
// SSH sessions: one for the main agent and one for the subagent. If the
// subagent wrongly fell back to the first (local) channel, the count would be
// one.
func TestSubagentInheritsMainChannel(t *testing.T) {
	modelSrv, _ := scriptedModelServer(t)
	sshSrv, err := harness.New("/bin/bash")
	if err != nil {
		t.Fatalf("harness.New: %v", err)
	}
	t.Cleanup(func() { sshSrv.Close() })

	dir := t.TempDir()
	mainPrompt := filepath.Join(dir, "prompt.md")
	coderPrompt := filepath.Join(dir, "coder-prompt.md")
	if err := os.WriteFile(mainPrompt, []byte("be helpful"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(coderPrompt, []byte("be a coder"), 0o600); err != nil {
		t.Fatal(err)
	}

	toml := fmt.Sprintf(`
[models.testmodel]
provider = "openai_responses"
base_url = %q
api_key  = "test"
model    = "m"

[[channels]]
name = "local"
type = "local"

[[channels]]
name = "devbox"
type = "ssh"
host = "127.0.0.1"
port = %d
user = "test"
auth = { password = "test" }
insecure = true
keep_alive = true
default_shell = "/bin/bash"

[agents.main]
model = "testmodel"
system_prompt = %q
channel = "devbox"
tools = "all"
allow_subagents = true

[agents.coder]
model = "testmodel"
system_prompt = %q
tools = "all"

[approval]
mode = "ask"
`, modelSrv.URL, sshSrv.Port(), mainPrompt, coderPrompt)

	cfgPath := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(cfgPath, []byte(toml), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := config.Validate(cfg, config.ValidateOptions{
		KnownProviders: map[string]bool{
			config.ProviderOpenAIResponses:       true,
			config.ProviderOpenAIChatCompletions: true,
		},
		KnownChannelTypes: map[string]bool{config.ChannelTypeSSH: true, config.ChannelTypeLocal: true},
		KnownTools:        map[string]bool{"execute_command": true, "set_approval": true},
	}); err != nil {
		t.Fatalf("Validate: %v", err)
	}

	rt, err := app.Build(context.Background(), cfg, app.Options{Agent: "main"})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	t.Cleanup(func() {
		if err := rt.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})

	// The main agent already opened its SSH session during Build.
	if got := sshSrv.SessionCount(); got != 1 {
		t.Fatalf("sessions after Build = %d, want 1 (main agent)", got)
	}

	id, err := rt.Manager.SpawnSubagent(context.Background(), rt.Agent.ID(), "coder", "", "task")
	if err != nil {
		t.Fatalf("SpawnSubagent: %v", err)
	}
	if rt.Manager.Agent(id) == nil {
		t.Fatalf("Manager.Agent(%q) = nil", id)
	}

	// The subagent must open its own session on the same (SSH) channel, not on
	// the first (local) channel.
	if got := sshSrv.SessionCount(); got != 2 {
		t.Fatalf("sessions after spawn = %d, want 2 (main + subagent)", got)
	}
}
