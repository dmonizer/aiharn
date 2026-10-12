package recorder

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"aiharn/internal/llm"
)

func writeArchivedFile(t *testing.T, home, key, firstLine string, entries ...string) {
	t.Helper()
	dir := filepath.Join(home, "transcripts")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	b.WriteString(firstLine)
	b.WriteString("\n")
	for _, entry := range entries {
		b.WriteString(entry)
		b.WriteString("\n")
	}
	if err := os.WriteFile(filepath.Join(dir, key), []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestArchivedSessionsMissingDir(t *testing.T) {
	home := t.TempDir()
	sessions, err := ArchivedSessions(home)
	if err != nil {
		t.Fatalf("ArchivedSessions() error = %v", err)
	}
	if sessions != nil {
		t.Fatalf("ArchivedSessions() = %#v, want nil", sessions)
	}
}

func TestArchivedSessionsListsNewestFirst(t *testing.T) {
	home := t.TempDir()
	writeArchivedFile(t, home, "older.jsonl",
		`{"type":"session","version":1,"started_at":"2024-01-01T00:00:00Z","model":"m1","agent":"main","channel":"c1","approval":"ask","session_id":"s1","session_name":"First"}`)
	writeArchivedFile(t, home, "newer.jsonl",
		`{"type":"session","version":1,"started_at":"2024-01-02T00:00:00Z","model":"m2","agent":"coder","channel":"c2","approval":"allow-all","session_id":"s2","session_name":"Second"}`)

	sessions, err := ArchivedSessions(home)
	if err != nil {
		t.Fatalf("ArchivedSessions() error = %v", err)
	}
	if len(sessions) != 2 {
		t.Fatalf("ArchivedSessions() = %d sessions, want 2", len(sessions))
	}
	if sessions[0].Key != "newer.jsonl" || sessions[1].Key != "older.jsonl" {
		t.Fatalf("sessions not newest-first: %+v", sessions)
	}

	newer := sessions[0]
	if newer.ID != "s2" || newer.Name != "Second" || newer.Model != "m2" ||
		newer.AgentType != "coder" || newer.Channel != "c2" || newer.Approval != "allow-all" {
		t.Fatalf("newer session = %+v", newer)
	}
	if newer.StartedAt.Equal(time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC)) == false {
		t.Fatalf("newer started_at = %v", newer.StartedAt)
	}

	older := sessions[1]
	if older.ID != "s1" || older.Name != "First" || older.Model != "m1" ||
		older.AgentType != "main" || older.Channel != "c1" || older.Approval != "ask" {
		t.Fatalf("older session = %+v", older)
	}
	if older.StartedAt.Equal(time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)) == false {
		t.Fatalf("older started_at = %v", older.StartedAt)
	}
}

func TestArchivedSessionsSkipsMalformedAndSharedTranscripts(t *testing.T) {
	home := t.TempDir()
	// A shared --log transcript has no session_id and must be skipped.
	writeArchivedFile(t, home, "shared.jsonl",
		`{"type":"session","version":1,"started_at":"2024-01-01T00:00:00Z","model":"m","agent":"main"}`)
	// A malformed header line is skipped.
	writeArchivedFile(t, home, "broken.jsonl", `not json`)
	// A non-transcript file is skipped outright.
	if err := os.WriteFile(filepath.Join(home, "transcripts", "notes.txt"), []byte("hi"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeArchivedFile(t, home, "ok.jsonl",
		`{"type":"session","version":1,"started_at":"2024-01-03T00:00:00Z","model":"m","agent":"main","channel":"c","approval":"ask","session_id":"s-ok"}`)

	sessions, err := ArchivedSessions(home)
	if err != nil {
		t.Fatalf("ArchivedSessions() error = %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("ArchivedSessions() = %+v, want only ok.jsonl", sessions)
	}
	if sessions[0].Key != "ok.jsonl" || sessions[0].ID != "s-ok" {
		t.Fatalf("session = %+v", sessions[0])
	}
}

func TestArchivedSessionDetailTopLevelMessagesOnly(t *testing.T) {
	home := t.TempDir()
	writeArchivedFile(t, home, "session.jsonl",
		`{"type":"session","version":1,"started_at":"2024-01-02T03:04:05.123456789Z","model":"m","agent":"main","channel":"local","approval":"ask","session_id":"abc123","session_name":"Archived"}`,
		`{"time":"2024-01-02T03:04:06Z","type":"user","agent_id":"main","agent_type":"main","content":"hello"}`,
		`{"time":"2024-01-02T03:04:07Z","type":"assistant","agent_id":"main","agent_type":"main","content":"reply"}`,
		`{"time":"2024-01-02T03:04:08Z","type":"user","agent_id":"coder-1","agent_type":"coder","content":"sub task"}`,
		`{"time":"2024-01-02T03:04:09Z","type":"assistant","agent_id":"coder-1","agent_type":"coder","content":"sub reply"}`,
		`{"time":"2024-01-02T03:04:10Z","type":"tool_call","agent_id":"main","agent_type":"main","call_id":"c1","name":"t"}`,
		`{"time":"2024-01-02T03:04:11Z","type":"reasoning","agent_id":"main","agent_type":"main","content":"thinking"}`)

	detail, err := ArchivedSessionDetail(home, "session.jsonl", 0)
	if err != nil {
		t.Fatalf("ArchivedSessionDetail() error = %v", err)
	}
	if detail.Key != "session.jsonl" || detail.ID != "abc123" || detail.Name != "Archived" {
		t.Fatalf("detail header = %+v", detail.ArchivedSession)
	}
	if len(detail.Messages) != 2 {
		t.Fatalf("messages = %+v, want 2 top-level messages", detail.Messages)
	}
	if detail.Messages[0] != (ArchivedMessage{Type: "message", Role: "user", Content: "hello"}) {
		t.Fatalf("messages[0] = %+v", detail.Messages[0])
	}
	if detail.Messages[1] != (ArchivedMessage{Type: "message", Role: "assistant", Content: "reply"}) {
		t.Fatalf("messages[1] = %+v", detail.Messages[1])
	}

	limited, err := ArchivedSessionDetail(home, "session.jsonl", 1)
	if err != nil {
		t.Fatalf("ArchivedSessionDetail(limit=1) error = %v", err)
	}
	if len(limited.Messages) != 1 || limited.Messages[0].Role != "user" {
		t.Fatalf("limited messages = %+v, want only the user message", limited.Messages)
	}
}

func TestArchivedSessionDetailNotFound(t *testing.T) {
	home := t.TempDir()
	writeArchivedFile(t, home, "session.jsonl",
		`{"type":"session","version":1,"started_at":"2024-01-02T03:04:05Z","agent":"main","session_id":"abc123"}`)

	if _, err := ArchivedSessionDetail(home, "../x.jsonl", 0); !errors.Is(err, ErrArchivedNotFound) {
		t.Fatalf("ArchivedSessionDetail(../x.jsonl) error = %v, want ErrArchivedNotFound", err)
	}
	if _, err := ArchivedSessionDetail(home, "missing.jsonl", 0); !errors.Is(err, ErrArchivedNotFound) {
		t.Fatalf("ArchivedSessionDetail(missing.jsonl) error = %v, want ErrArchivedNotFound", err)
	}
	if _, err := ArchivedSessionDetail(home, "", 0); !errors.Is(err, ErrArchivedNotFound) {
		t.Fatalf("ArchivedSessionDetail(\"\") error = %v, want ErrArchivedNotFound", err)
	}
}

func TestArchivedHistoryReconstructsTopLevel(t *testing.T) {
	home := t.TempDir()
	writeArchivedFile(t, home, "session.jsonl",
		`{"type":"session","version":1,"started_at":"2024-01-02T03:04:05Z","model":"m","agent":"main","channel":"local","approval":"ask","session_id":"abc123","session_name":"Archived"}`,
		`{"type":"user","agent_id":"main","content":"hello"}`,
		`{"type":"tool_call","agent_id":"main","call_id":"c1","name":"execute_command","arguments":{"command":"pwd"}}`,
		`{"type":"tool_result","agent_id":"main","call_id":"c1","content":"/home"}`,
		`{"type":"assistant","agent_id":"main","content":"done"}`,
		`{"type":"user","agent_id":"coder-1","content":"sub task"}`,
		`{"type":"assistant","agent_id":"coder-1","content":"sub reply"}`,
		`{"type":"reasoning","agent_id":"main","content":"thinking"}`,
		`{"type":"text_delta","agent_id":"main","content":"partial"}`,
		`{"type":"state","agent_id":"main","content":"idle"}`)

	history, meta, err := ArchivedHistory(home, "session.jsonl")
	if err != nil {
		t.Fatalf("ArchivedHistory() error = %v", err)
	}
	if meta.ID != "abc123" || meta.Name != "Archived" || meta.Model != "m" ||
		meta.AgentType != "main" || meta.Channel != "local" || meta.Approval != "ask" {
		t.Fatalf("meta = %+v", meta)
	}
	if len(history) != 4 {
		t.Fatalf("history = %+v, want 4 items", history)
	}
	if history[0].Type != llm.ItemMessage || history[0].Role != llm.RoleUser || history[0].Content != "hello" {
		t.Fatalf("history[0] = %+v", history[0])
	}
	if history[1].Type != llm.ItemFunctionCall || history[1].CallID != "c1" ||
		history[1].Name != "execute_command" || history[1].Args != `{"command":"pwd"}` {
		t.Fatalf("history[1] = %+v", history[1])
	}
	if history[2].Type != llm.ItemFunctionCallOutput || history[2].CallID != "c1" || history[2].Content != "/home" {
		t.Fatalf("history[2] = %+v", history[2])
	}
	if history[3].Type != llm.ItemMessage || history[3].Role != llm.RoleAssistant || history[3].Content != "done" {
		t.Fatalf("history[3] = %+v", history[3])
	}
}

func TestArchivedHistoryDefaultsAgentAndValidatesKey(t *testing.T) {
	home := t.TempDir()
	writeArchivedFile(t, home, "session.jsonl",
		`{"type":"session","version":1,"session_id":"s1","session_name":"No Agent"}`,
		`{"type":"user","agent_id":"main","content":"hi"}`,
		`{"type":"assistant","agent_id":"main","content":"yo"}`)

	history, meta, err := ArchivedHistory(home, "session.jsonl")
	if err != nil {
		t.Fatalf("ArchivedHistory() error = %v", err)
	}
	if meta.AgentType != "main" {
		t.Fatalf("AgentType = %q, want main", meta.AgentType)
	}
	if len(history) != 2 {
		t.Fatalf("history = %+v, want 2 items", history)
	}

	if _, _, err := ArchivedHistory(home, "../x.jsonl"); !errors.Is(err, ErrArchivedNotFound) {
		t.Fatalf("ArchivedHistory(../x.jsonl) error = %v, want ErrArchivedNotFound", err)
	}
	if _, _, err := ArchivedHistory(home, "missing.jsonl"); !errors.Is(err, ErrArchivedNotFound) {
		t.Fatalf("ArchivedHistory(missing.jsonl) error = %v, want ErrArchivedNotFound", err)
	}
	if _, _, err := ArchivedHistory(home, ""); !errors.Is(err, ErrArchivedNotFound) {
		t.Fatalf("ArchivedHistory(\"\") error = %v, want ErrArchivedNotFound", err)
	}
}
