package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"aiharn/internal/llm"
	"aiharn/internal/logging"
)

// NameSetSessionTitle is the tool name for setting the conversation session's
// agent-authored title, shown in the web session list beneath the user-set name.
const NameSetSessionTitle = "set_session_title"

// SessionTitleBackend applies an agent-chosen title to the conversation session
// that owns the calling agent. It is declared here (not imported from app) to
// keep the tools→app dependency acyclic; app.Session implements it.
type SessionTitleBackend interface {
	SetSessionTitle(title string) error
}

// SetSessionTitle returns the tool that lets the model set a short title for
// the current conversation session. The title is display-only metadata.
func SetSessionTitle(backend SessionTitleBackend) Tool {
	return &setSessionTitle{backend: backend}
}

type setSessionTitle struct {
	backend SessionTitleBackend
}

func (t *setSessionTitle) Definition() llm.ToolDefinition {
	return llm.ToolDefinition{
		Name:        NameSetSessionTitle,
		Description: "Set a concise title (at most 150 characters) for the current conversation session. The title is shown in the web console's session list beneath the user-set name.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"title": {"type": "string", "minLength": 1, "maxLength": 150, "description": "A short, human-readable title for the session."}
			},
			"required": ["title"],
			"additionalProperties": false
		}`),
	}
}

func (t *setSessionTitle) Run(ctx context.Context, args json.RawMessage) (string, error) {
	var p struct {
		Title string `json:"title"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return "", fmt.Errorf("parse arguments: %w", err)
	}
	title := strings.TrimSpace(p.Title)
	if title == "" {
		return "", errors.New("title must not be empty")
	}
	logging.Debug("tool: set_session_title",
		slog.String("component", "tool"),
		slog.String("tool", NameSetSessionTitle),
		slog.String("title", title),
	)
	if t.backend == nil {
		return "", errors.New("session title is not available")
	}
	if err := t.backend.SetSessionTitle(title); err != nil {
		logging.Debug("tool: set_session_title result",
			slog.String("component", "tool"),
			slog.String("tool", NameSetSessionTitle),
			slog.Any("err", err))
		return "", err
	}
	logging.Debug("tool: set_session_title result",
		slog.String("component", "tool"),
		slog.String("tool", NameSetSessionTitle),
		slog.String("title", title))
	return fmt.Sprintf("session title set to %q", title), nil
}
