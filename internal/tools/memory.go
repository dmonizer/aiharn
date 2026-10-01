package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"aiharn/internal/llm"
	"aiharn/internal/logging"
	"aiharn/internal/memory"
)

// Memory tool names.
const (
	NameWriteMemory  = "write_memory"
	NameListMemories = "list_memories"
	NameGetMemory    = "get_memory"
)

// WriteMemory returns the tool that stores a memory in the local (per-session)
// or global (shared) scope. The store allocates a unique index for every write.
func WriteMemory(backend memory.Backend) Tool {
	return &writeMemory{backend: backend}
}

type writeMemory struct {
	backend memory.Backend
}

func (t *writeMemory) Definition() llm.ToolDefinition {
	return llm.ToolDefinition{
		Name:        NameWriteMemory,
		Description: "Store a memory the model can retrieve later. local memories are private to the current conversation session; global memories are shared across sessions. The system assigns a unique index.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"scope": {"type": "string", "enum": ["local", "global"], "description": "Where to store the memory."},
				"summary": {"type": "string", "description": "A short label for the memory, shown by list_memories."},
				"content": {"type": "string", "description": "The full memory content to store."}
			},
			"required": ["scope", "summary", "content"],
			"additionalProperties": false
		}`),
	}
}

func (t *writeMemory) Run(ctx context.Context, args json.RawMessage) (string, error) {
	var p struct {
		Scope   string `json:"scope"`
		Summary string `json:"summary"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return "", fmt.Errorf("parse arguments: %w", err)
	}
	scope, err := memory.ParseScope(p.Scope)
	if err != nil {
		return "", err
	}
	logging.Debug("tool: write_memory",
		slog.String("component", "tool"),
		slog.String("tool", NameWriteMemory),
		slog.String("scope", string(scope)),
		slog.String("summary", p.Summary),
		slog.Int("content_bytes", len(p.Content)),
	)
	entry, err := t.backend.WriteMemory(scope, p.Summary, p.Content)
	if err != nil {
		logging.Debug("tool: write_memory result", slog.String("component", "tool"), slog.String("tool", NameWriteMemory), slog.Any("err", err))
		return "", err
	}
	logging.Debug("tool: write_memory result", slog.String("component", "tool"), slog.String("tool", NameWriteMemory), slog.Int("index", entry.Index), slog.String("scope", entry.Scope))
	return fmt.Sprintf("wrote memory %d (%s): %s", entry.Index, entry.Scope, entry.Summary), nil
}

// ListMemories returns the tool that lists memory indexes and summaries.
func ListMemories(backend memory.Backend) Tool {
	return &listMemories{backend: backend}
}

type listMemories struct {
	backend memory.Backend
}

func (t *listMemories) Definition() llm.ToolDefinition {
	return llm.ToolDefinition{
		Name:        NameListMemories,
		Description: "List memory indexes and summaries. Use with get_memory to read the full content. The scope defaults to local.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"scope": {"type": "string", "enum": ["local", "global"], "description": "Which memories to list. Defaults to local."}
			},
			"additionalProperties": false
		}`),
	}
}

func (t *listMemories) Run(ctx context.Context, args json.RawMessage) (string, error) {
	var p struct {
		Scope string `json:"scope"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return "", fmt.Errorf("parse arguments: %w", err)
	}
	scope := memory.ScopeLocal
	if p.Scope != "" {
		parsed, err := memory.ParseScope(p.Scope)
		if err != nil {
			return "", err
		}
		scope = parsed
	}
	logging.Debug("tool: list_memories",
		slog.String("component", "tool"),
		slog.String("tool", NameListMemories),
		slog.String("scope", string(scope)),
	)
	entries, err := t.backend.ListMemories(scope)
	if err != nil {
		logging.Debug("tool: list_memories result", slog.String("component", "tool"), slog.String("tool", NameListMemories), slog.Any("err", err))
		return "", err
	}
	logging.Debug("tool: list_memories result", slog.String("component", "tool"), slog.String("tool", NameListMemories), slog.Int("count", len(entries)))
	if len(entries) == 0 {
		return "no memories", nil
	}
	lines := make([]string, 0, len(entries))
	for _, entry := range entries {
		lines = append(lines, fmt.Sprintf("- index=%d summary=%s", entry.Index, entry.Summary))
	}
	return strings.Join(lines, "\n"), nil
}

// GetMemory returns the tool that reads one stored memory by index.
func GetMemory(backend memory.Backend) Tool {
	return &getMemory{backend: backend}
}

type getMemory struct {
	backend memory.Backend
}

func (t *getMemory) Definition() llm.ToolDefinition {
	return llm.ToolDefinition{
		Name:        NameGetMemory,
		Description: "Return the full content of the memory at the given index. The index is shown by list_memories and is unique across both local and global memories.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"index": {"type": "integer", "description": "The memory index returned by list_memories."}
			},
			"required": ["index"],
			"additionalProperties": false
		}`),
	}
}

func (t *getMemory) Run(ctx context.Context, args json.RawMessage) (string, error) {
	var p struct {
		Index int `json:"index"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return "", fmt.Errorf("parse arguments: %w", err)
	}
	logging.Debug("tool: get_memory",
		slog.String("component", "tool"),
		slog.String("tool", NameGetMemory),
		slog.Int("index", p.Index),
	)
	entry, err := t.backend.GetMemory(p.Index)
	if err != nil {
		logging.Debug("tool: get_memory result", slog.String("component", "tool"), slog.String("tool", NameGetMemory), slog.Any("err", err))
		return "", err
	}
	logging.Debug("tool: get_memory result", slog.String("component", "tool"), slog.String("tool", NameGetMemory), slog.Int("index", entry.Index), slog.String("scope", entry.Scope))
	return fmt.Sprintf("index: %d\nscope: %s\nsummary: %s\ncontent:\n%s", entry.Index, entry.Scope, entry.Summary, entry.Content), nil
}
