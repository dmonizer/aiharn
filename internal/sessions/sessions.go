// Package sessions defines the conversation-session contract shared by
// internal/app, which owns the sessions, and internal/webapi, which exposes
// them. It holds only interfaces, one header struct, and sentinel errors, so
// both sides can depend on it without importing each other.
//
// A conversation session is one top-level agent plus its subagent tree, its
// retained history, its approval gate, its transcript, and its pending-message
// queue. It is unrelated to internal/execution.Session, which is a persistent
// execution shell owned by one agent.
package sessions

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	"aiharn/internal/agent"
	"aiharn/internal/approval"
	"aiharn/internal/llm"
)

// Channel is one configured execution channel exposed to clients.
type Channel struct {
	Name string
	Type string
}

// Agent is the slice of a session's top-level agent needed by the remote API.
// *agent.Agent satisfies it.
type Agent interface {
	ID() string
	// Name is the agent's human-readable display name, distinct from the
	// session's Handle.Name(). It is always non-empty: an agent configured
	// without a name reports its ID instead.
	Name() string
	Type() string
	State() agent.State
	History() []llm.Item
	Turn(context.Context, string) error
}

// Handle is one conversation session: a top-level agent, its subagent tree, its
// retained history, its approval gate, and its pending-message queue.
type Handle interface {
	ID() string
	Name() string
	CreatedAt() time.Time
	Model() string
	Channel() string
	Agent() Agent
	Manager() *agent.Manager
	Gate() *approval.Gate
	// Submit enqueues content for this session. An empty agentID addresses the
	// top-level agent; otherwise agentID must name a subagent of this session.
	Submit(ctx context.Context, agentID, content string) error
	Queued() int
	LastError() string
	// SetChannel switches the session's active execution channel for subsequent
	// commands. The change applies to the session's top-level agent.
	SetChannel(ctx context.Context, name string) error
}

// Store owns every session in the process. Implemented by app.SessionManager.
type Store interface {
	Default() Handle
	List() []Handle
	Create(ctx context.Context, name string) (Handle, error)
	Lookup(id string) (Handle, bool)
	Rename(id, name string) (Handle, error)
	Close(ctx context.Context, id string) error
	// Channels returns every configured execution channel, in config order.
	Channels() []Channel
}

// Transcript is the per-session transcript sink: every history item appended by
// any agent in the session, plus the session header metadata.
type Transcript interface {
	agent.HistoryObserver
	SetMeta(TranscriptMeta)
	Close() error
}

// TranscriptMeta is the header of one session transcript.
type TranscriptMeta struct {
	ID, Name, Model, AgentType, Channel, Approval string
}

// Sentinel errors returned by the session layer. They carry a "session: "
// prefix, like the agent and webapi packages.
var (
	ErrNotFound        = errors.New("session: not found")
	ErrLimitReached    = errors.New("session: limit reached")
	ErrClosed          = errors.New("session: closed")
	ErrQueueFull       = errors.New("session: message queue is full")
	ErrDefault         = errors.New("session: default session cannot be closed")
	ErrAgentNotFound   = errors.New("session: agent not found")
	ErrNameInvalid     = errors.New("session: invalid name")
	ErrChannelNotFound = errors.New("session: channel not found")
)

// NameLimitBytes bounds a session name.
const NameLimitBytes = 64

// ValidateName trims surrounding space and rejects an empty name, a name
// containing control characters, and a name longer than NameLimitBytes. It
// returns the trimmed name. Rejections wrap ErrNameInvalid.
func ValidateName(name string) (string, error) {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return "", fmt.Errorf("%w: name must not be empty", ErrNameInvalid)
	}
	if len(trimmed) > NameLimitBytes {
		return "", fmt.Errorf("%w: name exceeds %d bytes", ErrNameInvalid, NameLimitBytes)
	}
	if strings.ContainsFunc(trimmed, unicode.IsControl) {
		return "", fmt.Errorf("%w: name must not contain control characters", ErrNameInvalid)
	}
	return trimmed, nil
}
