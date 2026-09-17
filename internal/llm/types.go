// Package llm defines the provider-neutral contract between Aiharn's agent core
// and any LLM backend. Implementations translate this contract to a specific
// wire protocol; the rest of the program never imports SDK types.
package llm

import (
	"context"
	"encoding/json"
)

// Role identifies the speaker of a message item.
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

// Origin records who authored a message item. It is internal bookkeeping: the
// provider adapters build requests field by field (role, content), so it is
// never sent to a model.
type Origin string

const (
	OriginHuman Origin = "human" // typed by a person (console, terminal UI)
	OriginAgent Origin = "agent" // produced by the agent system, e.g. a delivered subagent report
)

// Delivery records agent-to-agent message provenance: which agent sent text to
// which other agent, in which direction, and why. It is set only on message
// items another agent injected. Like Origin, it is internal bookkeeping: the
// provider adapters build requests field by field (role, content), so it is
// never sent to a model.
type Delivery struct {
	From      string // sending agent id
	To        string // receiving agent id
	Direction string // DirectionUp or DirectionDown, describing the sender's position
	Kind      string // KindTask, KindReport, or KindMessage
}

// Delivery direction values. They describe where the message travelled relative
// to its SENDER: a subagent replying to its caller sends "up" (an ancestor),
// and a caller delegating to a new subagent sends "down" (a descendant).
const (
	DirectionUp   = "up"
	DirectionDown = "down"
)

// Delivery kind values, so a UI can label a message by what it is for.
const (
	KindTask    = "task"    // a spawn prompt: the caller's instructions to a new subagent
	KindReport  = "report"  // a subagent's final result, delivered exactly once
	KindMessage = "message" // a mid-conversation message from send_agent_message
)

// ItemType discriminates the kinds of transcript entries.
type ItemType string

const (
	ItemMessage            ItemType = "message"
	ItemFunctionCall       ItemType = "function_call"
	ItemFunctionCallOutput ItemType = "function_call_output"
)

// Item is a normalized, provider-neutral transcript entry. The agent is the
// single owner of conversation history; Items are rebuilt into a full stateless
// request on every call.
type Item struct {
	Type    ItemType
	Role    Role   // message items only
	Content string // message text, or function-call output value
	CallID  string // function-call id (call and its output)
	Name    string // function name (function_call)
	Args    string // raw JSON arguments (function_call)
	// Origin records who authored a message item (human vs. agent). It is empty
	// for non-message items and for message items of unknown provenance, such as
	// older recordings made before origins were tracked.
	Origin Origin // message items only
	// Delivery is set only on message items that another agent injected. It
	// lets a UI show who sent what to whom, and in which direction, instead of
	// rendering an agent-to-agent message as a person's own text.
	Delivery *Delivery // message items only
}

// ToolDefinition describes a callable tool to the model.
type ToolDefinition struct {
	Name        string
	Description string
	Parameters  json.RawMessage // JSON Schema
}

// Request is a complete, stateless chat request.
type Request struct {
	Model            string
	System           string // system prompt / instructions
	Stream           bool
	Input            []Item // full history, oldest first
	Tools            []ToolDefinition
	ReasoningEffort  string // optional; supported values are model-dependent
	ReasoningSummary string // Responses API summary mode: auto, concise, or detailed
}

// Usage reports token consumption for a completed response.
type Usage struct {
	InputTokens  int
	OutputTokens int
	TotalTokens  int
}

// EventType discriminates stream events.
type EventType int

const (
	EventTextDelta EventType = iota
	EventReasoningDelta
	EventCompleted // terminal: normalized output Items + Usage + FinishReason
	EventFailed    // terminal: stream/provider error
)

// Event is a single normalized event emitted on the stream.
type Event struct {
	Type         EventType
	Text         string // TextDelta or ReasoningDelta
	Items        []Item // Completed: output items to append to history
	Usage        Usage
	FinishReason string // Completed: "stop" or an incomplete reason
	Err          error  // Failed
}

// Client streams a chat completion.
//
// Contract: setup/request failures are returned synchronously. After a non-nil
// channel is returned, the producer emits zero or more text/reasoning events, then
// exactly one terminal Completed or Failed event, then closes the channel.
// Cancelling ctx unblocks any blocked send and closes the channel promptly
// (without a terminal event).
type Client interface {
	Stream(ctx context.Context, req Request) (<-chan Event, error)
}
