// Package recorder persists a structured, provider-neutral log of an Aiharn
// session: user and assistant messages, tool calls and their outputs, and every
// subagent's own conversation, tagged with the agent that produced each entry.
// Output is JSON Lines (NDJSON) — one JSON object per line — so the log can be
// streamed incrementally and survives an unclean shutdown.
package recorder

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"aiharn/internal/agent"
	"aiharn/internal/llm"
	"aiharn/internal/sessions"
)

// Meta is the session-level description written as the log's header line.
type Meta struct {
	Model       string
	AgentType   string
	Channel     string
	Approval    string
	SessionID   string `json:"session_id,omitempty"`
	SessionName string `json:"session_name,omitempty"`
}

// Entry is one NDJSON line. Type discriminates completed history items from
// streamed deltas, full tool output and agent lifecycle/task events.
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

// Recorder implements the per-session transcript sink.
var _ sessions.Transcript = (*Recorder)(nil)

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

// NewSessionFile creates a new, private transcript below
// <aiharn_home>/transcripts. UTC nanoseconds in the filename and O_EXCL prevent
// a new session from overwriting an earlier transcript.
func NewSessionFile(aiharnHome string) (*Recorder, string, error) {
	dir := filepath.Join(aiharnHome, "transcripts")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, "", err
	}
	name := time.Now().UTC().Format("2006-01-02T15-04-05.000000000Z") + ".jsonl"
	path := filepath.Join(dir, name)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, "", err
	}
	r := New(f)
	r.closer = f
	return r, path, nil
}

// SetSession records the session description used in the header line. Call it
// before the first ObserveHistory so the header includes the metadata.
func (r *Recorder) SetSession(m Meta) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.meta = &m
}

// SetMeta implements sessions.Transcript, recording the session header used by
// the header line. Call it before the first ObserveHistory.
func (r *Recorder) SetMeta(m sessions.TranscriptMeta) {
	r.SetSession(Meta{
		Model:       m.Model,
		AgentType:   m.AgentType,
		Channel:     m.Channel,
		Approval:    m.Approval,
		SessionID:   m.ID,
		SessionName: m.Name,
	})
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

// ObserveEvent records streaming activity independently of the completed
// history. In particular, tool_output_full is the actual UI-visible result,
// even when the result subsequently passed to the model is truncated.
func (r *Recorder) ObserveEvent(agentID, agentType string, event agent.Event) {
	e := Entry{Time: time.Now(), AgentID: agentID, AgentType: agentType}
	switch event.Type {
	case agent.EventText:
		e.Type, e.Content = "text_delta", event.Text
	case agent.EventReasoningStart:
		e.Type = "reasoning_start"
	case agent.EventReasoningDelta:
		e.Type, e.Content = "reasoning_delta", event.Text
	case agent.EventTaskQueued:
		e.Type, e.Content = "task_queued", event.Text
	case agent.EventPause:
		e.Type, e.Content = "pause", event.Text
	case agent.EventToolResult:
		e.Type, e.Content, e.CallID, e.Name = "tool_output_full", event.Text, event.Call.CallID, event.Call.Name
	case agent.EventState:
		e.Type, e.Content = "state", event.State.String()
	case agent.EventTimeout:
		e.Type, e.Content = "timeout", event.Text
	case agent.EventToolLimit:
		if event.ToolLimit == nil {
			return
		}
		e.Type = "tool_limit"
		e.Content = fmt.Sprintf("%d/%d", event.ToolLimit.Count, event.ToolLimit.Limit)
	default:
		return // user messages and tool calls are already in completed history
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ensureHeaderLocked()
	r.enc.Encode(e)
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
		// Emitted only for per-session transcripts; a shared transcript has no
		// single session identity.
		if r.meta.SessionID != "" {
			h["session_id"] = r.meta.SessionID
		}
		if r.meta.SessionName != "" {
			h["session_name"] = r.meta.SessionName
		}
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
	case llm.ItemReasoning:
		e.Type = "reasoning"
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
