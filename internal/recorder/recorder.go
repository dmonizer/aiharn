// Package recorder persists a structured, provider-neutral log of an Aiharn
// session: user and assistant messages, tool calls and their outputs, and every
// subagent's own conversation, tagged with the agent that produced each entry.
// Output is JSON Lines (NDJSON) — one JSON object per line — so the log can be
// streamed incrementally and survives an unclean shutdown.
package recorder

import (
	"encoding/json"
	"io"
	"os"
	"sync"
	"time"

	"aiharn/internal/llm"
)

// Meta is the session-level description written as the log's header line.
type Meta struct {
	Model     string
	AgentType string
	Channel   string
	Approval  string
}

// Entry is one NDJSON line. Type discriminates the record kind:
// "session", "user", "assistant", "system", "tool_call", or "tool_result".
type Entry struct {
	Time      time.Time       `json:"time"`
	Type      string          `json:"type"`
	AgentID   string          `json:"agent_id"`
	AgentType string          `json:"agent_type"`
	Content   string          `json:"content,omitempty"`
	CallID    string          `json:"call_id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

// Recorder writes one Entry per line to an underlying writer. It is safe for
// concurrent use by every agent in a session.
type Recorder struct {
	mu            sync.Mutex
	enc           *json.Encoder
	closer        io.Closer
	startedAt     time.Time
	meta          *Meta
	headerWritten bool
}

// New returns a Recorder writing NDJSON to w. w is not closed by Close.
func New(w io.Writer) *Recorder {
	return &Recorder{
		enc:       json.NewEncoder(w),
		startedAt: time.Now(),
	}
}

// NewFile returns a Recorder writing to path, created (or truncated). The file
// is closed by Close.
func NewFile(path string) (*Recorder, error) {
	f, err := os.Create(path)
	if err != nil {
		return nil, err
	}
	r := New(f)
	r.closer = f
	return r, nil
}

// SetSession records the session description used in the header line. Call it
// before the first ObserveHistory so the header includes the metadata.
func (r *Recorder) SetSession(m Meta) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.meta = &m
}

// ObserveHistory implements agent.HistoryObserver, appending one Entry per item.
func (r *Recorder) ObserveHistory(agentID, agentType string, items []llm.Item) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ensureHeaderLocked()
	for _, it := range items {
		r.enc.Encode(entryFromItem(agentID, agentType, it))
	}
}

// Close closes the underlying file, if any. It is idempotent.
func (r *Recorder) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closer == nil {
		return nil
	}
	err := r.closer.Close()
	r.closer = nil
	return err
}

// ensureHeaderLocked writes the session header line exactly once. Callers hold r.mu.
func (r *Recorder) ensureHeaderLocked() {
	if r.headerWritten {
		return
	}
	r.headerWritten = true
	h := map[string]any{
		"type":       "session",
		"version":    1,
		"started_at": r.startedAt,
	}
	if r.meta != nil {
		h["model"] = r.meta.Model
		h["agent"] = r.meta.AgentType
		h["channel"] = r.meta.Channel
		h["approval"] = r.meta.Approval
	}
	r.enc.Encode(h)
}

func entryFromItem(agentID, agentType string, it llm.Item) Entry {
	e := Entry{
		Time:      time.Now(),
		AgentID:   agentID,
		AgentType: agentType,
	}
	switch it.Type {
	case llm.ItemMessage:
		e.Type = string(it.Role)
		e.Content = it.Content
	case llm.ItemFunctionCall:
		e.Type = "tool_call"
		e.CallID = it.CallID
		e.Name = it.Name
		e.Arguments = rawArgs(it.Args)
	case llm.ItemFunctionCallOutput:
		e.Type = "tool_result"
		e.CallID = it.CallID
		e.Content = it.Content
	}
	return e
}

// rawArgs returns it.Args as raw JSON when it is already valid, and otherwise as
// a JSON string literal so the emitted line always parses.
func rawArgs(s string) json.RawMessage {
	if s == "" {
		return nil
	}
	if json.Valid([]byte(s)) {
		return json.RawMessage(s)
	}
	b, _ := json.Marshal(s)
	return json.RawMessage(b)
}
