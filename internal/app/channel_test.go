package app_test

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"aiharn/internal/app"
	"aiharn/internal/authfile"
	"aiharn/internal/config"
	"aiharn/internal/execution/ssh/harness"
	"aiharn/internal/webapi"
)

// TestSubagentInheritsMainChannel proves that a spawned agent runs on the main
// agent's channel even when its type has a different channel configured. The
// main agent uses the second channel (an SSH harness), while the coder type and
// first channel are local. After the spawn, the harness must have served two
// SSH sessions: one for the main agent and one for the subagent.
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
channel = "local"
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

	sessions, err := app.NewSessionManager(context.Background(), app.SessionManagerOptions{
		Config: cfg,
		Build:  app.Options{Agent: "main"},
	})
	if err != nil {
		t.Fatalf("NewSessionManager: %v", err)
	}
	t.Cleanup(func() {
		if err := sessions.Shutdown(); err != nil {
			t.Errorf("Shutdown: %v", err)
		}
	})
	rt := sessions.DefaultRuntime()
	if rt == nil {
		t.Fatal("DefaultRuntime = nil")
	}

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

// TestSubagentInheritsSwitchedMainChannel proves the web/TUI channel-switch
// path: the main agent starts on the default (first, local) channel, is then
// switched to an SSH channel at runtime, and a subagent spawned afterwards must
// inherit that switched channel — both in the catalog it advertises and in the
// transport it actually opens.
func TestSubagentInheritsSwitchedMainChannel(t *testing.T) {
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
tools = "all"
allow_subagents = true

[agents.coder]
model = "testmodel"
system_prompt = %q
channel = "local"
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

	sessions, err := app.NewSessionManager(context.Background(), app.SessionManagerOptions{
		Config: cfg,
		Build:  app.Options{Agent: "main"},
	})
	if err != nil {
		t.Fatalf("NewSessionManager: %v", err)
	}
	t.Cleanup(func() {
		if err := sessions.Shutdown(); err != nil {
			t.Errorf("Shutdown: %v", err)
		}
	})
	rt := sessions.DefaultRuntime()
	if rt == nil {
		t.Fatal("DefaultRuntime = nil")
	}

	// The main agent starts on the first (local) channel, so no SSH session
	// exists yet.
	if got := sshSrv.SessionCount(); got != 0 {
		t.Fatalf("sessions after Build = %d, want 0 (main on local)", got)
	}

	// Switch through the same API endpoint used by the web console.
	authPath := filepath.Join(dir, "users")
	if err := authfile.Set(authPath, "test", []byte("secret")); err != nil {
		t.Fatalf("create auth file: %v", err)
	}
	server, err := webapi.New(webapi.Config{
		Listen: "127.0.0.1:0", AuthFile: authPath, Sessions: sessions,
	})
	if err != nil {
		t.Fatalf("webapi.New: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/session/channel",
		bytes.NewBufferString(`{"channel":"devbox","session_id":"default"}`))
	req.Header.Set("Content-Type", "application/json")
	req.SetBasicAuth("test", "secret")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, req)
	if response.Code != http.StatusOK {
		t.Fatalf("channel switch status = %d: %s", response.Code, response.Body.String())
	}
	if got := rt.ChannelName(); got != "devbox" {
		t.Fatalf("channel after web switch = %q, want devbox", got)
	}
	if got := sshSrv.SessionCount(); got != 1 {
		t.Fatalf("sessions after switch = %d, want 1 (main controller)", got)
	}

	// list_subagent_types must advertise the switched channel, not either the
	// startup default or the subagent type's configured local channel, so the
	// model and the user see the channel a spawn will use.
	catalog, err := rt.Manager.ListSubagentTypes(context.Background(), rt.Agent.ID())
	if err != nil {
		t.Fatalf("ListSubagentTypes: %v", err)
	}
	coderChannel := ""
	for _, typ := range catalog.Types {
		if typ.Name == "coder" {
			coderChannel = typ.Channel
		}
	}
	if coderChannel != "devbox" {
		t.Fatalf("coder channel after switch = %q, want devbox", coderChannel)
	}

	id, err := rt.Manager.SpawnSubagent(context.Background(), rt.Agent.ID(), "coder", "", "task")
	if err != nil {
		t.Fatalf("SpawnSubagent: %v", err)
	}
	if rt.Manager.Agent(id) == nil {
		t.Fatalf("Manager.Agent(%q) = nil", id)
	}

	// The subagent must open its own session on the switched (SSH) channel.
	if got := sshSrv.SessionCount(); got != 2 {
		t.Fatalf("sessions after spawn = %d, want 2 (main + subagent both ssh)", got)
	}
}
