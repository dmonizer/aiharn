package app_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"aiharn/internal/app"
	"aiharn/internal/config"
	"aiharn/internal/execution/ssh/harness"
	"aiharn/internal/memory"
)

// TestBuildAgentPropagatesSpawnSpecName proves that the runtime wiring copies a
// SpawnSpec's Name into the built agent. The Manager builds every subagent
// through the same buildAgent seam, so observing the requested Name on a
// spawned agent is evidence that buildAgent propagated spec.Name.
//
// No provider or network is needed: the scripted httptest model server and the
// in-process SSH harness back real config load/validate and real wiring,
// exactly as TestVerticalSlice does. No turn is run — the assertions only read
// the names of the built agents.
func TestBuildAgentPropagatesSpawnSpecName(t *testing.T) {
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

	// Like writeConfig, but with a second agent type ([agents.coder]) to spawn
	// and allow_subagents = true on [agents.main] (it defaults to false, which
	// would otherwise refuse the spawn).
	toml := fmt.Sprintf(`
[models.testmodel]
provider = "openai_responses"
base_url = %q
api_key  = "test"
model    = "m"

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
channel = "devbox"
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

	memMgr, err := memory.NewManager("", "")
	if err != nil {
		t.Fatalf("memory.NewManager: %v", err)
	}
	rt, err := app.Build(context.Background(), cfg, app.Options{Agent: "main", Memory: memMgr})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	t.Cleanup(func() {
		if err := rt.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})

	ctx := context.Background()

	// The top-level agent is built from a SpawnSpec with no name, so its Name()
	// falls back to its id and must never be empty.
	if got := rt.Agent.Name(); got == "" {
		t.Fatalf("top-level agent Name() is empty, want its id %q", rt.Agent.ID())
	} else if got != rt.Agent.ID() {
		t.Fatalf("top-level agent Name() = %q, want its id %q", got, rt.Agent.ID())
	}

	// A named spawn must reach the built agent — the buildAgent propagation proof.
	id, err := rt.Manager.SpawnSubagent(ctx, rt.Agent.ID(), "coder", "researcher", "task")
	if err != nil {
		t.Fatalf("SpawnSubagent(named): %v", err)
	}
	sub := rt.Manager.Agent(id)
	if sub == nil {
		t.Fatalf("Manager.Agent(%q) = nil", id)
	}
	if got := sub.Name(); got != "researcher" {
		t.Errorf("named subagent Name() = %q, want %q", got, "researcher")
	}

	// An empty name defaults to the generated id (coder-N).
	id2, err := rt.Manager.SpawnSubagent(ctx, rt.Agent.ID(), "coder", "", "task")
	if err != nil {
		t.Fatalf("SpawnSubagent(unnamed): %v", err)
	}
	if !strings.HasPrefix(id2, "coder-") {
		t.Fatalf("unnamed subagent id = %q, want a coder-N id", id2)
	}
	sub2 := rt.Manager.Agent(id2)
	if sub2 == nil {
		t.Fatalf("Manager.Agent(%q) = nil", id2)
	}
	if got := sub2.Name(); got != id2 {
		t.Errorf("unnamed subagent Name() = %q, want the generated id %q", got, id2)
	}
}
