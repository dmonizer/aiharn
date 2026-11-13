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
	"testing"
	"time"

	"aiharn/internal/app"
	"aiharn/internal/approval"
	"aiharn/internal/config"
	"aiharn/internal/filetools"
	"aiharn/internal/memory"
)

// fileToolConfig writes a config with one local channel rooted at root and the
// text file tools enabled. approvalMode selects the gate policy.
func fileToolConfig(t *testing.T, dir, root, modelURL, approvalMode string) *config.Config {
	t.Helper()
	promptPath := filepath.Join(dir, "prompt.md")
	if err := os.WriteFile(promptPath, []byte("be helpful"), 0o600); err != nil {
		t.Fatal(err)
	}
	toml := fmt.Sprintf(`
aiharn_home = %q

[models.testmodel]
provider = "openai_responses"
base_url = %q
api_key  = "test"
model    = "m"

[[channels]]
name = "local"
type = "local"
root = %q

[agents.main]
model = "testmodel"
system_prompt = "prompt.md"
channel = "local"
tools = "all"

[approval]
mode = %q
`, dir, modelURL, root, approvalMode)
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
		KnownTools:        toolNameSet(),
	}); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	return cfg
}

// scriptedFileModelServer answers each request with the next scripted function
// call and finally with a plain message. args may be derived from the request
// body, so later calls can use a read_id returned by an earlier tool result.
type scriptedFileModelServer struct {
	srv  *httptest.Server
	step int
}

func newScriptedFileModelServer(t *testing.T, scripts ...func(body []byte) (name, args string)) *scriptedFileModelServer {
	t.Helper()
	s := &scriptedFileModelServer{}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		usage := map[string]any{"input_tokens": 5, "output_tokens": 5, "total_tokens": 10}
		if s.step < len(scripts) {
			name, args := scripts[s.step](body)
			s.step++
			io.WriteString(w, sseFrame(map[string]any{
				"type": "response.completed",
				"response": map[string]any{
					"status": "completed",
					"output": []any{map[string]any{
						"type": "function_call", "call_id": fmt.Sprintf("call_%d", s.step),
						"name": name, "arguments": args,
					}},
					"usage": usage,
				},
			}))
		} else {
			s.step++
			io.WriteString(w, sseFrame(map[string]any{
				"type": "response.completed",
				"response": map[string]any{
					"status": "completed",
					"output": []any{map[string]any{
						"type": "message", "role": "assistant",
						"content": []any{map[string]any{"type": "output_text", "text": "done"}},
					}},
					"usage": usage,
				},
			}))
		}
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(s.srv.Close)
	return s
}

// extractReadID finds the read_id of the most recent read result in a request
// body, including results embedded as JSON strings.
func extractReadID(body []byte) string {
	var v any
	if err := json.Unmarshal(body, &v); err != nil {
		return ""
	}
	return findReadID(v)
}

func findReadID(v any) string {
	switch value := v.(type) {
	case map[string]any:
		if id, ok := value["read_id"].(string); ok {
			return id
		}
		for _, item := range value {
			if id := findReadID(item); id != "" {
				return id
			}
		}
	case []any:
		for _, item := range value {
			if id := findReadID(item); id != "" {
				return id
			}
		}
	case string:
		var nested any
		if json.Unmarshal([]byte(value), &nested) == nil {
			return findReadID(nested)
		}
	}
	return ""
}

func toolNameSet() map[string]bool {
	set := map[string]bool{}
	for _, name := range []string{"execute_command", "write_memory", "list_memories", "get_memory",
		"set_session_title", "list_subagent_types", "spawn_subagent", "send_subagent_message",
		"send_agent_message", "check_subagent", "list_subagents", "close_subagent", "set_approval",
		"read", "write", "edit"} {
		set[name] = true
	}
	return set
}

func TestFileToolsReadThenEditTurn(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "note.txt"), []byte("alpha\nbeta\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	model := newScriptedFileModelServer(t,
		func([]byte) (string, string) {
			return "read", `{"filename":"note.txt","start_line":0,"end_line":2}`
		},
		func(body []byte) (string, string) {
			id := extractReadID(body)
			if id == "" {
				return "read", `{"filename":"note.txt"}`
			}
			return "edit", fmt.Sprintf(`{"filename":"note.txt","read_id":%q,"start_line":1,"end_line":2,"content":"BETA\n"}`, id)
		},
	)
	cfg := fileToolConfig(t, t.TempDir(), root, model.srv.URL, "allow-all")
	rt, err := app.Build(context.Background(), cfg, app.Options{
		Agent:        "main",
		Memory:       mustMemory(t),
		FileRegistry: filetools.NewRegistry(),
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := rt.Agent.Turn(ctx, "edit the file"); err != nil {
		t.Fatalf("Turn: %v", err)
	}
	content, err := os.ReadFile(filepath.Join(root, "note.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "alpha\nBETA\n" {
		t.Fatalf("content = %q", content)
	}
	hist := rt.Agent.History()
	if len(hist) != 6 {
		t.Fatalf("history len = %d: %+v", len(hist), hist)
	}
	if !strings.Contains(hist[2].Content, `"read_id"`) {
		t.Fatalf("read result = %q", hist[2].Content)
	}
	if !strings.Contains(hist[4].Content, `"line_count"`) {
		t.Fatalf("edit result = %q", hist[4].Content)
	}
}

func TestFileToolsWriteApprovalPrompt(t *testing.T) {
	root := t.TempDir()
	model := newScriptedFileModelServer(t, func([]byte) (string, string) {
		return "write", `{"filename":"created.txt","content":"hello\nworld\n"}`
	})
	cfg := fileToolConfig(t, t.TempDir(), root, model.srv.URL, "ask")
	rt, err := app.Build(context.Background(), cfg, app.Options{
		Agent:        "main",
		Memory:       mustMemory(t),
		FileRegistry: filetools.NewRegistry(),
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	turnDone := make(chan error, 1)
	go func() { turnDone <- rt.Agent.Turn(ctx, "create a file") }()

	var req approval.Request
	select {
	case req = <-rt.Gate.Pending():
	case <-ctx.Done():
		t.Fatal("no approval request surfaced")
	}
	if req.ToolName != "write" || !strings.Contains(req.Command, "create") || !strings.Contains(req.Command, "+ hello") {
		t.Fatalf("approval request = %+v", req)
	}
	if err := rt.Gate.Decide(req.ID, approval.DecisionDenied); err != nil {
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
	if _, err := os.Stat(filepath.Join(root, "created.txt")); !os.IsNotExist(err) {
		t.Fatalf("denied write created a file: %v", err)
	}
	hist := rt.Agent.History()
	if len(hist) < 3 || !strings.Contains(hist[2].Content, filetools.CodeApprovalDenied) {
		t.Fatalf("history = %+v", hist)
	}
}

func mustMemory(t *testing.T) *memory.Manager {
	t.Helper()
	mgr, err := memory.NewManager("", "")
	if err != nil {
		t.Fatalf("memory.NewManager: %v", err)
	}
	return mgr
}

// TestFileToolsCommandReadDoesNotAuthorizeEdit proves that reading a file
// through execute_command grants no edit context: the tools accept only their
// own opaque receipts.
func TestFileToolsCommandReadDoesNotAuthorizeEdit(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "note.txt"), []byte("alpha\nbeta\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	model := newScriptedFileModelServer(t,
		func([]byte) (string, string) {
			return "execute_command", `{"command":"cat note.txt"}`
		},
		func([]byte) (string, string) {
			return "edit", `{"filename":"note.txt","read_id":"0000000000000000","start_line":0,"end_line":1,"content":"X\n"}`
		},
	)
	cfg := fileToolConfig(t, t.TempDir(), root, model.srv.URL, "allow-all")
	rt, err := app.Build(context.Background(), cfg, app.Options{
		Agent:        "main",
		Memory:       mustMemory(t),
		FileRegistry: filetools.NewRegistry(),
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := rt.Agent.Turn(ctx, "edit without reading"); err != nil {
		t.Fatalf("Turn: %v", err)
	}
	content, err := os.ReadFile(filepath.Join(root, "note.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "alpha\nbeta\n" {
		t.Fatalf("file changed without a receipt: %q", content)
	}
	hist := rt.Agent.History()
	found := false
	for _, item := range hist {
		if strings.Contains(item.Content, filetools.CodeReadRequired) {
			found = true
		}
	}
	if !found {
		t.Fatalf("no read_required failure in history: %+v", hist)
	}
}
