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

// ItemType discriminates the kinds of transcript entries.
type ItemType string

const (
	ItemMessage             ItemType = "message"
	ItemFunctionCall        ItemType = "function_call"
	ItemFunctionCallOutput  ItemType = "function_call_output"
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
}

// ToolDefinition describes a callable tool to the model.
type ToolDefinition struct {
	Name        string
	Description string
	Parameters  json.RawMessage // JSON Schema
}

// Request is a complete, stateless chat request.
type Request struct {
	Model  string
	System string // system prompt / instructions
	Stream bool
	Input  []Item // full history, oldest first
	Tools  []ToolDefinition
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
	EventCompleted           // terminal: normalized output Items + Usage + FinishReason
	EventFailed              // terminal: stream/provider error
)

// Event is a single normalized event emitted on the stream.
type Event struct {
	Type         EventType
	Text         string // TextDelta
	Items        []Item // Completed: output items to append to history
	Usage        Usage
	FinishReason string // Completed: "stop" or an incomplete reason
	Err          error  // Failed
}

// Client streams a chat completion.
//
// Contract: setup/request failures are returned synchronously. After a non-nil
// channel is returned, the producer emits zero or more TextDelta events, then
// exactly one terminal Completed or Failed event, then closes the channel.
// Cancelling ctx unblocks any blocked send and closes the channel promptly
// (without a terminal event).
type Client interface {
	Stream(ctx context.Context, req Request) (<-chan Event, error)
}
