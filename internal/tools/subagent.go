package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"aiharn/internal/approval"
	"aiharn/internal/llm"
	"aiharn/internal/logging"
)

// Subagent tool names.
const (
	NameSpawnSubagent       = "spawn_subagent"
	NameListSubagentTypes   = "list_subagent_types"
	NameSendSubagentMessage = "send_subagent_message"
	NameCheckSubagent       = "check_subagent"
	NameListSubagents       = "list_subagents"
	NameCloseSubagent       = "close_subagent"
)

// SubagentType describes one configured type that spawn_subagent accepts.
type SubagentType struct {
	Name           string
	Description    string
	Model          string
	Channel        string
	AllowSubagents bool
}

// SubagentCatalog describes the configured types and the caller's current
// ability to spawn one. ActiveAgents includes in-flight spawn reservations.
type SubagentCatalog struct {
	Types          []SubagentType
	CanSpawn       bool
	BlockedReasons []string
	CallerDepth    int
	MaxDepth       int
	ActiveAgents   int
	MaxOpenAgents  int
}

// SubagentStatus is the status snapshot the subagent tools report to the model.
type SubagentStatus struct {
	ID     string
	Type   string
	State  string
	Depth  int
	Tail   string // recent final output, may be empty
	Paused bool   // queued tasks are held; an in-flight turn finishes normally
}

// SubagentBackend is the runtime capability the subagent tools require. It is
// deliberately narrow so the tools package has no knowledge of the agent runtime;
// the agent package's Manager implements it.
type SubagentBackend interface {
	SpawnSubagent(ctx context.Context, callerID, agentType, prompt string) (string, error)
	ListSubagentTypes(ctx context.Context, callerID string) (SubagentCatalog, error)
	SendSubagentMessage(ctx context.Context, callerID, subagentID, message string) error
	CheckSubagent(ctx context.Context, callerID, subagentID string) (SubagentStatus, error)
	ListSubagents(ctx context.Context, callerID string) ([]SubagentStatus, error)
	CloseSubagent(ctx context.Context, callerID, subagentID string) error
}

// ListSubagentTypes returns the read-only tool that describes which configured
// agent types spawn_subagent accepts and whether the caller can spawn one now.
// spawnToolEnabled reflects the caller's selected tool set, in addition to the
// backend's live manager constraints.
func ListSubagentTypes(backend SubagentBackend, callerID string, spawnToolEnabled bool) Tool {
	return &listSubagentTypes{backend: backend, callerID: callerID, spawnToolEnabled: spawnToolEnabled}
}

type listSubagentTypes struct {
	backend          SubagentBackend
	callerID         string
	spawnToolEnabled bool
}

func (t *listSubagentTypes) Definition() llm.ToolDefinition {
	return llm.ToolDefinition{
		Name: NameListSubagentTypes,
		Description: "List the configured subagent types accepted by spawn_subagent and report the caller's current " +
			"depth, capacity, and allow_subagents constraints. Use this before choosing an agent_type.",
		Parameters: json.RawMessage(`{"type": "object", "additionalProperties": false}`),
	}
}

func (t *listSubagentTypes) Run(ctx context.Context, args json.RawMessage) (string, error) {
	logging.Debug("tool: list_subagent_types",
		slog.String("component", "tool"),
		slog.String("tool", NameListSubagentTypes),
		slog.String("caller_id", t.callerID),
	)
	catalog, err := t.backend.ListSubagentTypes(ctx, t.callerID)
	if err != nil {
		logging.Debug("tool: list_subagent_types result", slog.String("component", "tool"), slog.String("tool", NameListSubagentTypes), slog.Any("err", err))
		return "", err
	}
	if !t.spawnToolEnabled {
		catalog.CanSpawn = false
		catalog.BlockedReasons = append(catalog.BlockedReasons, "spawn_subagent tool is not enabled for caller")
	}
	logging.Debug("tool: list_subagent_types result",
		slog.String("component", "tool"),
		slog.String("tool", NameListSubagentTypes),
		slog.Int("type_count", len(catalog.Types)),
		slog.Bool("can_spawn", catalog.CanSpawn),
	)
	return formatSubagentCatalog(catalog), nil
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
	logging.Debug("tool: spawn_subagent",
		slog.String("component", "tool"),
		slog.String("tool", NameSpawnSubagent),
		slog.String("caller_id", t.callerID),
		slog.String("agent_type", p.AgentType),
		slog.Int("prompt_bytes", len(p.Prompt)),
	)

	d, err := t.gate.Check(ctx, approval.Request{
		ToolName: NameSpawnSubagent,
		Command:  p.AgentType,
		Args:     string(args),
	})
	if err != nil {
		logging.Debug("tool: spawn_subagent result", slog.String("component", "tool"), slog.String("tool", NameSpawnSubagent), slog.Any("err", err))
		return "", err
	}
	if d == approval.DecisionDenied {
		logging.Debug("tool: spawn_subagent result", slog.String("component", "tool"), slog.String("tool", NameSpawnSubagent), slog.String("decision", "denied"))
		return "denied by user", nil
	}

	id, err := t.backend.SpawnSubagent(ctx, t.callerID, p.AgentType, p.Prompt)
	if err != nil {
		logging.Debug("tool: spawn_subagent result", slog.String("component", "tool"), slog.String("tool", NameSpawnSubagent), slog.Any("err", err))
		return "", err
	}
	logging.Debug("tool: spawn_subagent result", slog.String("component", "tool"), slog.String("tool", NameSpawnSubagent), slog.String("subagent_id", id))
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
	logging.Debug("tool: send_subagent_message",
		slog.String("component", "tool"),
		slog.String("tool", NameSendSubagentMessage),
		slog.String("caller_id", t.callerID),
		slog.String("subagent_id", p.SubagentID),
		slog.Int("message_bytes", len(p.Message)),
	)
	if err := t.backend.SendSubagentMessage(ctx, t.callerID, p.SubagentID, p.Message); err != nil {
		logging.Debug("tool: send_subagent_message result", slog.String("component", "tool"), slog.String("tool", NameSendSubagentMessage), slog.String("subagent_id", p.SubagentID), slog.Any("err", err))
		return "", err
	}
	logging.Debug("tool: send_subagent_message result", slog.String("component", "tool"), slog.String("tool", NameSendSubagentMessage), slog.String("subagent_id", p.SubagentID))
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
	logging.Debug("tool: check_subagent",
		slog.String("component", "tool"),
		slog.String("tool", NameCheckSubagent),
		slog.String("caller_id", t.callerID),
		slog.String("subagent_id", p.SubagentID),
	)
	s, err := t.backend.CheckSubagent(ctx, t.callerID, p.SubagentID)
	if err != nil {
		logging.Debug("tool: check_subagent result", slog.String("component", "tool"), slog.String("tool", NameCheckSubagent), slog.String("subagent_id", p.SubagentID), slog.Any("err", err))
		return "", err
	}
	logging.Debug("tool: check_subagent result", slog.String("component", "tool"), slog.String("tool", NameCheckSubagent), slog.String("subagent_id", p.SubagentID), slog.String("state", s.State))
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
	logging.Debug("tool: list_subagents",
		slog.String("component", "tool"),
		slog.String("tool", NameListSubagents),
		slog.String("caller_id", t.callerID),
	)
	subs, err := t.backend.ListSubagents(ctx, t.callerID)
	if err != nil {
		logging.Debug("tool: list_subagents result", slog.String("component", "tool"), slog.String("tool", NameListSubagents), slog.Any("err", err))
		return "", err
	}
	logging.Debug("tool: list_subagents result", slog.String("component", "tool"), slog.String("tool", NameListSubagents), slog.Int("count", len(subs)))
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
	logging.Debug("tool: close_subagent",
		slog.String("component", "tool"),
		slog.String("tool", NameCloseSubagent),
		slog.String("caller_id", t.callerID),
		slog.String("subagent_id", p.SubagentID),
	)
	if err := t.backend.CloseSubagent(ctx, t.callerID, p.SubagentID); err != nil {
		logging.Debug("tool: close_subagent result", slog.String("component", "tool"), slog.String("tool", NameCloseSubagent), slog.String("subagent_id", p.SubagentID), slog.Any("err", err))
		return "", err
	}
	logging.Debug("tool: close_subagent result", slog.String("component", "tool"), slog.String("tool", NameCloseSubagent), slog.String("subagent_id", p.SubagentID))
	return fmt.Sprintf("closed subagent %s", p.SubagentID), nil
}

func formatSubagentStatus(s SubagentStatus) string {
	out := fmt.Sprintf("id=%s type=%s state=%s depth=%d", s.ID, s.Type, s.State, s.Depth)
	if s.Paused {
		out += " paused=true"
	}
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
		line := fmt.Sprintf("- id=%s type=%s state=%s depth=%d", s.ID, s.Type, s.State, s.Depth)
		if s.Paused {
			line += " paused=true"
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

func formatSubagentCatalog(c SubagentCatalog) string {
	lines := make([]string, 0, len(c.Types)+3)
	if len(c.Types) == 0 {
		lines = append(lines, "configured subagent types: none")
	} else {
		lines = append(lines, "configured subagent types:")
		for _, typ := range c.Types {
			line := fmt.Sprintf("- name=%s model=%s channel=%s allow_subagents=%t",
				typ.Name, typ.Model, typ.Channel, typ.AllowSubagents)
			if typ.Description != "" {
				line += " description=" + strings.ReplaceAll(typ.Description, "\n", " ")
			}
			lines = append(lines, line)
		}
	}
	remaining := c.MaxOpenAgents - c.ActiveAgents
	if remaining < 0 {
		remaining = 0
	}
	lines = append(lines, fmt.Sprintf("current constraints: caller_depth=%d max_depth=%d active_agents=%d max_open_agents=%d slots_remaining=%d",
		c.CallerDepth, c.MaxDepth, c.ActiveAgents, c.MaxOpenAgents, remaining))
	if c.CanSpawn {
		lines = append(lines, "spawn_available=true (spawn_subagent remains subject to the current approval policy)")
	} else {
		reason := "unavailable"
		if len(c.BlockedReasons) > 0 {
			reason = strings.Join(c.BlockedReasons, "; ")
		}
		lines = append(lines, "spawn_available=false reason="+reason)
	}
	return strings.Join(lines, "\n")
}
