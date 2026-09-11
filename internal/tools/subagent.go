package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"aiharn/internal/approval"
	"aiharn/internal/llm"
)

// Subagent tool names.
const (
	NameSpawnSubagent       = "spawn_subagent"
	NameSendSubagentMessage = "send_subagent_message"
	NameCheckSubagent       = "check_subagent"
	NameListSubagents       = "list_subagents"
	NameCloseSubagent       = "close_subagent"
)

// SubagentStatus is the status snapshot the subagent tools report to the model.
type SubagentStatus struct {
	ID    string
	Type  string
	State string
	Depth int
	Tail  string // recent final output, may be empty
}

// SubagentBackend is the runtime capability the subagent tools require. It is
// deliberately narrow so the tools package has no knowledge of the agent runtime;
// the agent package's Manager implements it.
type SubagentBackend interface {
	SpawnSubagent(ctx context.Context, callerID, agentType, prompt string) (string, error)
	SendSubagentMessage(ctx context.Context, callerID, subagentID, message string) error
	CheckSubagent(ctx context.Context, callerID, subagentID string) (SubagentStatus, error)
	ListSubagents(ctx context.Context, callerID string) ([]SubagentStatus, error)
	CloseSubagent(ctx context.Context, callerID, subagentID string) error
}

// SpawnSubagent returns the tool that creates a subagent of a given type. It is
// gated by approval; the tool is bound to the caller that owns it.
func SpawnSubagent(backend SubagentBackend, gate *approval.Gate, callerID string) Tool {
	return &spawnSubagent{backend: backend, gate: gate, callerID: callerID}
}

type spawnSubagent struct {
	backend  SubagentBackend
	gate     *approval.Gate
	callerID string
}

func (t *spawnSubagent) Definition() llm.ToolDefinition {
	return llm.ToolDefinition{
		Name:        NameSpawnSubagent,
		Description: "Spawn a subagent of a configured agent type and give it an initial task. Requires user approval. Returns the new subagent's id.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"agent_type": {"type": "string", "description": "The configured agent type to spawn."},
				"prompt": {"type": "string", "description": "The initial task for the subagent."}
			},
			"required": ["agent_type", "prompt"],
			"additionalProperties": false
		}`),
	}
}

func (t *spawnSubagent) Run(ctx context.Context, args json.RawMessage) (string, error) {
	var p struct {
		AgentType string `json:"agent_type"`
		Prompt    string `json:"prompt"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return "", fmt.Errorf("parse arguments: %w", err)
	}

	d, err := t.gate.Check(ctx, approval.Request{
		ToolName: NameSpawnSubagent,
		Command:  p.AgentType,
		Args:     string(args),
	})
	if err != nil {
		return "", err
	}
	if d == approval.DecisionDenied {
		return "denied by user", nil
	}

	id, err := t.backend.SpawnSubagent(ctx, t.callerID, p.AgentType, p.Prompt)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("spawned subagent %s", id), nil
}

// SendSubagentMessage returns the tool that enqueues a message to a subagent's
// inbox. It is exempt from approval.
func SendSubagentMessage(backend SubagentBackend, callerID string) Tool {
	return &sendSubagentMessage{backend: backend, callerID: callerID}
}

type sendSubagentMessage struct {
	backend  SubagentBackend
	callerID string
}

func (t *sendSubagentMessage) Definition() llm.ToolDefinition {
	return llm.ToolDefinition{
		Name:        NameSendSubagentMessage,
		Description: "Send a message (a follow-up task) to an open subagent. The subagent processes it on its next turn.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"subagent_id": {"type": "string", "description": "The subagent id returned by spawn_subagent."},
				"message": {"type": "string", "description": "The message to send."}
			},
			"required": ["subagent_id", "message"],
			"additionalProperties": false
		}`),
	}
}

func (t *sendSubagentMessage) Run(ctx context.Context, args json.RawMessage) (string, error) {
	var p struct {
		SubagentID string `json:"subagent_id"`
		Message    string `json:"message"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return "", fmt.Errorf("parse arguments: %w", err)
	}
	if err := t.backend.SendSubagentMessage(ctx, t.callerID, p.SubagentID, p.Message); err != nil {
		return "", err
	}
	return fmt.Sprintf("message sent to %s", p.SubagentID), nil
}

// CheckSubagent returns the tool that reports a subagent's status.
func CheckSubagent(backend SubagentBackend, callerID string) Tool {
	return &checkSubagent{backend: backend, callerID: callerID}
}

type checkSubagent struct {
	backend  SubagentBackend
	callerID string
}

func (t *checkSubagent) Definition() llm.ToolDefinition {
	return llm.ToolDefinition{
		Name:        NameCheckSubagent,
		Description: "Report a subagent's status and its most recent output.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"subagent_id": {"type": "string", "description": "The subagent id returned by spawn_subagent."}
			},
			"required": ["subagent_id"],
			"additionalProperties": false
		}`),
	}
}

func (t *checkSubagent) Run(ctx context.Context, args json.RawMessage) (string, error) {
	var p struct {
		SubagentID string `json:"subagent_id"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return "", fmt.Errorf("parse arguments: %w", err)
	}
	s, err := t.backend.CheckSubagent(ctx, t.callerID, p.SubagentID)
	if err != nil {
		return "", err
	}
	return formatSubagentStatus(s), nil
}

// ListSubagents returns the tool that lists all subagents.
func ListSubagents(backend SubagentBackend, callerID string) Tool {
	return &listSubagents{backend: backend, callerID: callerID}
}

type listSubagents struct {
	backend  SubagentBackend
	callerID string
}

func (t *listSubagents) Definition() llm.ToolDefinition {
	return llm.ToolDefinition{
		Name:        NameListSubagents,
		Description: "List all subagents and their statuses.",
		Parameters:  json.RawMessage(`{"type": "object", "additionalProperties": false}`),
	}
}

func (t *listSubagents) Run(ctx context.Context, args json.RawMessage) (string, error) {
	subs, err := t.backend.ListSubagents(ctx, t.callerID)
	if err != nil {
		return "", err
	}
	return formatSubagentList(subs), nil
}

// CloseSubagent returns the tool that closes a subagent and its descendants.
func CloseSubagent(backend SubagentBackend, callerID string) Tool {
	return &closeSubagent{backend: backend, callerID: callerID}
}

type closeSubagent struct {
	backend  SubagentBackend
	callerID string
}

func (t *closeSubagent) Definition() llm.ToolDefinition {
	return llm.ToolDefinition{
		Name:        NameCloseSubagent,
		Description: "Close a subagent (and any subagents it spawned), releasing its resources.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"subagent_id": {"type": "string", "description": "The subagent id returned by spawn_subagent."}
			},
			"required": ["subagent_id"],
			"additionalProperties": false
		}`),
	}
}

func (t *closeSubagent) Run(ctx context.Context, args json.RawMessage) (string, error) {
	var p struct {
		SubagentID string `json:"subagent_id"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return "", fmt.Errorf("parse arguments: %w", err)
	}
	if err := t.backend.CloseSubagent(ctx, t.callerID, p.SubagentID); err != nil {
		return "", err
	}
	return fmt.Sprintf("closed subagent %s", p.SubagentID), nil
}

func formatSubagentStatus(s SubagentStatus) string {
	out := fmt.Sprintf("id=%s type=%s state=%s depth=%d", s.ID, s.Type, s.State, s.Depth)
	if s.Tail != "" {
		out += "\nlast output: " + s.Tail
	}
	return out
}

func formatSubagentList(subs []SubagentStatus) string {
	if len(subs) == 0 {
		return "no subagents"
	}
	lines := make([]string, 0, len(subs))
	for _, s := range subs {
		lines = append(lines, fmt.Sprintf("- id=%s type=%s state=%s depth=%d", s.ID, s.Type, s.State, s.Depth))
	}
	return strings.Join(lines, "\n")
}
