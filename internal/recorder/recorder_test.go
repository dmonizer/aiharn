package recorder

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"aiharn/internal/agent"
	"aiharn/internal/llm"
	"aiharn/internal/sessions"
)

func decodeLines(t *testing.T, data []byte) []map[string]any {
	t.Helper()
	var out []map[string]any
	for i, line := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("line %d is not valid JSON: %v (%q)", i, err, line)
		}
		out = append(out, m)
	}
	return out
}

func TestRecorderEmitsNDJSON(t *testing.T) {
	var buf bytes.Buffer
	r := New(&buf)
	r.SetSession(Meta{
		Model: "m", AgentType: "main", Channel: "c", Approval: "ask",
		SessionID: "s1", SessionName: "Session 1",
	})

	r.ObserveHistory("main", "main", []llm.Item{
		{Type: llm.ItemMessage, Role: llm.RoleUser, Content: "hi"},
		{Type: llm.ItemMessage, Role: llm.RoleAssistant, Content: "hello"},
		{Type: llm.ItemFunctionCall, CallID: "c1", Name: "execute_command", Args: `{"command":"ls"}`},
		{Type: llm.ItemFunctionCallOutput, CallID: "c1", Content: "file.txt\n"},
	})

	lines := decodeLines(t, buf.Bytes())
	if len(lines) != 5 {
		t.Fatalf("got %d lines, want 5 (header + 4 items)", len(lines))
	}

	h := lines[0]
	if h["type"] != "session" || h["model"] != "m" || h["agent"] != "main" || h["approval"] != "ask" {
		t.Fatalf("header = %#v", h)
	}
	if h["session_id"] != "s1" || h["session_name"] != "Session 1" {
		t.Fatalf("header session fields = %#v", h)
	}

	if got := lines[1]["type"]; got != "user" {
		t.Fatalf("lines[1].type = %v", got)
	}
	if got := lines[2]["type"]; got != "assistant" {
		t.Fatalf("lines[2].type = %v", got)
	}

	tc := lines[3]
	if tc["type"] != "tool_call" || tc["call_id"] != "c1" || tc["name"] != "execute_command" {
		t.Fatalf("tool_call line = %#v", tc)
	}
	args, ok := tc["arguments"].(map[string]any)
	if !ok || args["command"] != "ls" {
		t.Fatalf("arguments = %#v", tc["arguments"])
	}

	tr := lines[4]
	if tr["type"] != "tool_result" || tr["call_id"] != "c1" || tr["content"] != "file.txt\n" {
		t.Fatalf("tool_result line = %#v", tr)
	}
}

func TestRecorderHeaderWrittenOnce(t *testing.T) {
	var buf bytes.Buffer
	r := New(&buf)
	r.ObserveHistory("a", "t", []llm.Item{{Type: llm.ItemMessage, Role: llm.RoleUser, Content: "x"}})
	r.ObserveHistory("a", "t", []llm.Item{{Type: llm.ItemMessage, Role: llm.RoleAssistant, Content: "y"}})

	lines := decodeLines(t, buf.Bytes())
	sessions := 0
	for _, l := range lines {
		if l["type"] == "session" {
			sessions++
		}
	}
	if sessions != 1 {
		t.Fatalf("header written %d times, want 1", sessions)
	}
	if len(lines) != 3 {
		t.Fatalf("got %d lines, want 3", len(lines))
	}
}

func TestRecorderTagsAgent(t *testing.T) {
	var buf bytes.Buffer
	r := New(&buf)
	r.ObserveHistory("coder-1", "coder", []llm.Item{{Type: llm.ItemMessage, Role: llm.RoleAssistant, Content: "done"}})

	lines := decodeLines(t, buf.Bytes())
	last := lines[len(lines)-1]
	if last["agent_id"] != "coder-1" || last["agent_type"] != "coder" {
		t.Fatalf("last line = %#v", last)
	}
}

func TestRecorderSeparatesSubagentStreamAndFullToolOutput(t *testing.T) {
	var buf bytes.Buffer
	r := New(&buf)
	r.ObserveHistory("main", "main", []llm.Item{{Type: llm.ItemMessage, Role: llm.RoleUser, Content: "start"}})
	r.ObserveEvent("coder-1", "coder", agent.Event{Type: agent.EventTaskQueued, Text: "task"})
	r.ObserveEvent("coder-1", "coder", agent.Event{Type: agent.EventPause, Text: "paused"})
	r.ObserveHistory("coder-1", "coder", []llm.Item{{Type: llm.ItemMessage, Role: llm.RoleUser, Content: "task"}})
	r.ObserveEvent("coder-1", "coder", agent.Event{Type: agent.EventReasoningDelta, Text: "considering"})
	r.ObserveEvent("coder-1", "coder", agent.Event{Type: agent.EventText, Text: "partial answer"})
	r.ObserveEvent("coder-1", "coder", agent.Event{Type: agent.EventToolResult, Text: "the complete output", Call: llm.Item{CallID: "c1", Name: "execute_command"}})
	r.ObserveHistory("coder-1", "coder", []llm.Item{{Type: llm.ItemFunctionCallOutput, CallID: "c1", Content: "the complete [truncated]"}})
	lines := decodeLines(t, buf.Bytes())
	if len(lines) != 9 {
		t.Fatalf("entries = %+v", lines)
	}
	for i, typ := range []string{"task_queued", "pause", "user", "reasoning_delta", "text_delta", "tool_output_full", "tool_result"} {
		entry := lines[i+2]
		if entry["agent_id"] != "coder-1" || entry["agent_type"] != "coder" || entry["type"] != typ {
			t.Fatalf("entry %d = %#v, want coder-1/%s", i+2, entry, typ)
		}
	}
	if lines[7]["content"] != "the complete output" || lines[7]["call_id"] != "c1" {
		t.Fatalf("full output missing: %#v", lines[7])
	}
}

func TestRecorderInvalidArgsFallsBackToString(t *testing.T) {
	var buf bytes.Buffer
	r := New(&buf)
	r.ObserveHistory("main", "main", []llm.Item{{Type: llm.ItemFunctionCall, CallID: "c", Name: "t", Args: "not-json"}})

	lines := decodeLines(t, buf.Bytes())
	args := lines[len(lines)-1]["arguments"]
	if args != "not-json" {
		t.Fatalf("arguments = %#v, want string fallback", args)
	}
}

func TestCloseIdempotent(t *testing.T) {
	r := New(&bytes.Buffer{})
	if err := r.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestNewSessionFileCreatesPrivateTimestampedTranscripts(t *testing.T) {
	home := filepath.Join(t.TempDir(), "aiharn-home")
	first, firstPath, err := NewSessionFile(home)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = first.Close() })
	if got, want := filepath.Dir(firstPath), filepath.Join(home, "transcripts"); got != want {
		t.Fatalf("transcript directory = %q, want %q", got, want)
	}
	if !regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}-\d{2}-\d{2}\.\d{9}Z\.jsonl$`).MatchString(filepath.Base(firstPath)) {
		t.Fatalf("transcript filename is not timestamped: %q", firstPath)
	}
	info, err := os.Stat(firstPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("transcript permissions = %o, want 600", got)
	}
	dirInfo, err := os.Stat(filepath.Dir(firstPath))
	if err != nil {
		t.Fatal(err)
	}
	if got := dirInfo.Mode().Perm(); got != 0o700 {
		t.Fatalf("transcript directory permissions = %o, want 700", got)
	}
	first.ObserveHistory("main", "main", []llm.Item{{Type: llm.ItemMessage, Role: llm.RoleUser, Content: "first session"}})
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, secondPath, err := NewSessionFile(home)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = second.Close() })
	if firstPath == secondPath {
		t.Fatalf("two sessions shared transcript path %q", firstPath)
	}
	data, err := os.ReadFile(firstPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "first session") {
		t.Fatalf("first transcript was lost: %q", data)
	}
}

func TestRecorderSetMeta(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "transcript.jsonl")
	r, err := NewFile(path)
	if err != nil {
		t.Fatal(err)
	}
	r.SetMeta(sessions.TranscriptMeta{
		ID: "ab12cd34", Name: "Research", Model: "m",
		AgentType: "main", Channel: "c", Approval: "ask",
	})
	r.ObserveHistory("main", "main", []llm.Item{{Type: llm.ItemMessage, Role: llm.RoleUser, Content: "hi"}})
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	h := decodeLines(t, data)[0]
	if h["session_id"] != "ab12cd34" || h["session_name"] != "Research" {
		t.Fatalf("header = %#v", h)
	}
}

func TestRecorderOmitsUnsetSessionFields(t *testing.T) {
	var buf bytes.Buffer
	r := New(&buf)
	r.SetSession(Meta{Model: "m", AgentType: "main"})
	r.ObserveHistory("main", "main", []llm.Item{{Type: llm.ItemMessage, Role: llm.RoleUser, Content: "hi"}})

	h := decodeLines(t, buf.Bytes())[0]
	if _, ok := h["session_id"]; ok {
		t.Fatalf("shared transcript header carries session_id: %#v", h)
	}
	if _, ok := h["session_name"]; ok {
		t.Fatalf("shared transcript header carries session_name: %#v", h)
	}
}
