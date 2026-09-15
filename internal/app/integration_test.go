package app_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"aiharn/internal/app"
	"aiharn/internal/approval"
	"aiharn/internal/config"
	"aiharn/internal/execution/ssh/harness"
	"aiharn/internal/llm"
)

// sseFrame marshals v into one SSE data frame.
func sseFrame(v any) string {
	b, _ := json.Marshal(v)
	return "data: " + string(b) + "\n\n"
}

// scriptedModelServer returns an httptest server that answers the first request
// with a single execute_command tool call and every later request with a final
// assistant message "done". It counts requests.
func scriptedModelServer(t *testing.T) (*httptest.Server, *int32) {
	t.Helper()
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		n := atomic.AddInt32(&calls, 1)

		usage := map[string]any{"input_tokens": 5, "output_tokens": 5, "total_tokens": 10}
		if n == 1 {
			io.WriteString(w, sseFrame(map[string]any{
				"type": "response.completed",
				"response": map[string]any{
					"status": "completed",
					"output": []any{map[string]any{
						"type":      "function_call",
						"call_id":   "call_1",
						"name":      "execute_command",
						"arguments": `{"command":"printf 'hello-from-shell'"}`,
					}},
					"usage": usage,
				},
			}))
		} else {
			io.WriteString(w, sseFrame(map[string]any{
				"type": "response.completed",
				"response": map[string]any{
					"status": "completed",
					"output": []any{map[string]any{
						"type":    "message",
						"role":    "assistant",
						"content": []any{map[string]any{"type": "output_text", "text": "done"}},
					}},
					"usage": usage,
				},
			}))
		}
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

// writeConfig writes a config.toml and system prompt into dir, wired to the model
// server and the SSH harness. It returns the config path.
func writeConfig(t *testing.T, dir, modelURL string, port int) string {
	t.Helper()
	promptPath := filepath.Join(dir, "prompt.md")
	if err := os.WriteFile(promptPath, []byte("be helpful"), 0o600); err != nil {
		t.Fatal(err)
	}

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
system_prompt = "prompt.md"
channel = "devbox"
tools = "all"

[approval]
mode = "ask"
`, modelURL, port)

	cfgPath := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(cfgPath, []byte(toml), 0o600); err != nil {
		t.Fatal(err)
	}
	return cfgPath
}

// TestVerticalSlice drives the full stack end to end with no external network: a
// scripted Responses server supplies the model, and an in-process SSH harness
// runs the shell command, both behind real config load/validate and real wiring.
func TestVerticalSlice(t *testing.T) {
	modelSrv, modelCalls := scriptedModelServer(t)
	sshSrv, err := harness.New("/bin/bash")
	if err != nil {
		t.Fatalf("harness.New: %v", err)
	}
	t.Cleanup(func() { sshSrv.Close() })

	cfgPath := writeConfig(t, t.TempDir(), modelSrv.URL, sshSrv.Port())

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

	if rt.Summary.AgentType != "main" || rt.Summary.Channel != "devbox" || rt.Summary.Approval != "ask" {
		t.Fatalf("summary = %+v", rt.Summary)
	}

	// Run the turn; concurrently approve the one pending command.
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	turnDone := make(chan error, 1)
	go func() { turnDone <- rt.Agent.Turn(ctx, "run the command") }()

	var req approval.Request
	select {
	case req = <-rt.Gate.Pending():
	case <-ctx.Done():
		t.Fatal("no approval request surfaced")
	}
	if req.ToolName != "execute_command" || req.Command != `printf 'hello-from-shell'` {
		t.Fatalf("approval request = %+v", req)
	}
	if err := rt.Gate.Decide(req.ID, approval.DecisionApproved); err != nil {
		t.Fatalf("Decide: %v", err)
	}

	select {
	case err := <-turnDone:
		if err != nil {
			t.Fatalf("Turn: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("turn did not complete")
	}

	// The transcript reflects the full round trip.
	hist := rt.Agent.History()
	if len(hist) != 4 {
		t.Fatalf("history len = %d: %+v", len(hist), hist)
	}
	if hist[0].Role != llm.RoleUser || hist[0].Content != "run the command" {
		t.Errorf("history[0] = %+v", hist[0])
	}
	if hist[1].Type != llm.ItemFunctionCall || hist[1].Name != "execute_command" {
		t.Errorf("history[1] = %+v", hist[1])
	}
	if hist[2].Type != llm.ItemFunctionCallOutput || !strings.Contains(hist[2].Content, "hello-from-shell") {
		t.Errorf("history[2] = %+v, want shell output", hist[2])
	}
	if hist[3].Type != llm.ItemMessage || hist[3].Content != "done" {
		t.Errorf("history[3] = %+v", hist[3])
	}

	if got := atomic.LoadInt32(modelCalls); got != 2 {
		t.Errorf("model requests = %d, want 2", got)
	}

	// No server-side SSH errors surfaced.
	select {
	case err := <-sshSrv.Errors():
		t.Errorf("harness error: %v", err)
	default:
	}
}
