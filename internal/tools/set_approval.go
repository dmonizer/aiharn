package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"aiharn/internal/approval"
	"aiharn/internal/llm"
	"aiharn/internal/logging"
)

// SetApproval returns the tool that lets the model tighten the approval mode.
// The model may move allow-all → ask, never the reverse; the Gate enforces this.
func SetApproval(gate *approval.Gate) Tool {
	return &setApproval{gate: gate}
}

type setApproval struct {
	gate *approval.Gate
}

func (t *setApproval) Definition() llm.ToolDefinition {
	return llm.ToolDefinition{
		Name:        NameSetApproval,
		Description: "Tighten the command-approval mode. Only tightening (allow-all to ask) is permitted; the model cannot loosen approval.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"mode": {"type": "string", "enum": ["ask", "allow-all"]}
			},
			"required": ["mode"],
			"additionalProperties": false
		}`),
	}
}

func (t *setApproval) Run(ctx context.Context, args json.RawMessage) (string, error) {
	var p struct {
		Mode string `json:"mode"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return "", fmt.Errorf("parse arguments: %w", err)
	}
	logging.Debug("tool: set_approval",
		slog.String("component", "tool"),
		slog.String("tool", NameSetApproval),
		slog.String("mode", p.Mode),
	)
	var mode approval.Mode
	switch p.Mode {
	case "ask":
		mode = approval.ModeAsk
	case "allow-all":
		mode = approval.ModeAllowAll
	default:
		return "", fmt.Errorf("invalid mode %q", p.Mode)
	}
	if err := t.gate.ApplyModelMode(mode); err != nil {
		logging.Debug("tool: set_approval result", slog.String("component", "tool"), slog.String("tool", NameSetApproval), slog.Any("err", err))
		return "", err
	}
	logging.Debug("tool: set_approval result", slog.String("component", "tool"), slog.String("tool", NameSetApproval), slog.String("mode", mode.String()))
	return fmt.Sprintf("approval mode is now %s", mode), nil
}
