package recorder

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"aiharn/internal/llm"
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
	r.SetSession(Meta{Model: "m", AgentType: "main", Channel: "c", Approval: "ask"})

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
