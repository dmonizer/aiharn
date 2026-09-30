package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"aiharn/internal/approval"
	"aiharn/internal/execution"
	"aiharn/internal/llm"
	"aiharn/internal/logging"
)

// Executor runs a shell command. execution.Session satisfies it.
type Executor interface {
	Exec(ctx context.Context, cmd string, opts execution.ExecOptions) (execution.Result, error)
}

var _ Executor = (execution.Session)(nil)

// ExecuteCommand returns a tool that runs a shell command on the agent's
// executor, gated by the approval Gate. maxOutput caps retained stdout+stderr
// (0 = unlimited), and defaultCwd applies when the model omits cwd. The timeout
// starts after approval so user decision time does not consume command runtime.
// agentID and agentType identify the requester in the approval prompt.
func ExecuteCommand(ex Executor, gate *approval.Gate, maxOutput int64, defaultCwd string, timeout time.Duration, agentID, agentType string) Tool {
	return &execCommand{ex: ex, gate: gate, maxOutput: maxOutput, defaultCwd: defaultCwd, timeout: timeout, agentID: agentID, agentType: agentType}
}

type execCommand struct {
	ex         Executor
	gate       *approval.Gate
	maxOutput  int64
	defaultCwd string
	timeout    time.Duration
	agentID    string
	agentType  string
}

func (t *execCommand) Definition() llm.ToolDefinition {
	return llm.ToolDefinition{
		Name:        NameExecuteCommand,
		Description: "Run a shell command on the target machine and return its exit code, stdout, and stderr. Each command runs in a fresh shell: working directory, exported variables, functions, and shell options do not persist between calls (use the cwd argument or chain with && when needed). Requires user approval unless approval is allow-all.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"command": {"type": "string", "description": "The shell command to run."},
				"cwd": {"type": "string", "description": "Optional working directory."}
			},
			"required": ["command"],
			"additionalProperties": false
		}`),
	}
}

func (t *execCommand) Run(ctx context.Context, args json.RawMessage) (string, error) {
	start := time.Now()
	var p struct {
		Command string `json:"command"`
		Cwd     string `json:"cwd"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return "", fmt.Errorf("parse arguments: %w", err)
	}
	logging.Debug("tool: execute_command",
		slog.String("component", "tool"),
		slog.String("tool", NameExecuteCommand),
		slog.String("command", p.Command),
		slog.String("cwd", p.Cwd),
	)

	d, err := t.gate.Check(ctx, approval.Request{
		AgentID:   t.agentID,
		AgentType: t.agentType,
		ToolName:  NameExecuteCommand,
		Command:   p.Command,
		Args:      string(args),
	})
	if err != nil {
		logging.Debug("tool: execute_command result",
			slog.String("component", "tool"),
			slog.String("tool", NameExecuteCommand),
			slog.String("command", p.Command),
			slog.Int64("duration_ms", time.Since(start).Milliseconds()),
			slog.Any("err", err),
		)
		return "", err
	}
	decision := "approved"
	if d == approval.DecisionDenied {
		decision = "denied"
	}
	logging.Debug("tool: execute_command decision",
		slog.String("component", "tool"),
		slog.String("tool", NameExecuteCommand),
		slog.String("command", p.Command),
		slog.String("decision", decision),
	)
	if d == approval.DecisionDenied {
		return "denied by user", nil
	}

	cwd := p.Cwd
	if cwd == "" {
		cwd = t.defaultCwd
	}
	execCtx := ctx
	cancel := func() {}
	if t.timeout > 0 {
		execCtx, cancel = context.WithTimeout(ctx, t.timeout)
	}
	defer cancel()
	r, err := t.ex.Exec(execCtx, p.Command, execution.ExecOptions{
		Cwd:            cwd,
		MaxOutputBytes: t.maxOutput,
	})
	if err != nil {
		logging.Debug("tool: execute_command result",
			slog.String("component", "tool"),
			slog.String("tool", NameExecuteCommand),
			slog.String("command", p.Command),
			slog.String("cwd", cwd),
			slog.String("decision", decision),
			slog.Int64("duration_ms", time.Since(start).Milliseconds()),
			slog.Any("err", err),
		)
		return "", err
	}
	logging.Debug("tool: execute_command result",
		slog.String("component", "tool"),
		slog.String("tool", NameExecuteCommand),
		slog.String("command", p.Command),
		slog.String("cwd", cwd),
		slog.String("decision", decision),
		slog.Int("exit_code", r.ExitCode),
		slog.Int("stdout_bytes", len(r.Stdout)),
		slog.Int("stderr_bytes", len(r.Stderr)),
		slog.Bool("truncated", r.Truncated),
		slog.Int64("duration_ms", time.Since(start).Milliseconds()),
	)
	return formatExecResult(r), nil
}

func formatExecResult(r execution.Result) string {
	var b strings.Builder
	fmt.Fprintf(&b, "exit_code: %d\n", r.ExitCode)
	if r.Stdout != "" {
		b.WriteString("stdout:\n")
		b.WriteString(r.Stdout)
		if !strings.HasSuffix(r.Stdout, "\n") {
			b.WriteByte('\n')
		}
	}
	if r.Stderr != "" {
		b.WriteString("stderr:\n")
		b.WriteString(r.Stderr)
		if !strings.HasSuffix(r.Stderr, "\n") {
			b.WriteByte('\n')
		}
	}
	if r.Truncated {
		b.WriteString("[output truncated]\n")
	}
	return b.String()
}
