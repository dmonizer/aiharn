package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"aiharn/internal/approval"
	"aiharn/internal/execution"
	"aiharn/internal/llm"
)

// Executor runs a shell command. execution.Session satisfies it.
type Executor interface {
	Exec(ctx context.Context, cmd string, opts execution.ExecOptions) (execution.Result, error)
}

var _ Executor = (execution.Session)(nil)

// ExecuteCommand returns the tool that runs a shell command on the agent's
// executor, gated by the approval Gate. maxOutput caps retained stdout+stderr
// (0 = unlimited). defaultCwd is the directory used when the model omits cwd.
func ExecuteCommand(ex Executor, gate *approval.Gate, maxOutput int64, defaultCwd string) Tool {
	return &execCommand{ex: ex, gate: gate, maxOutput: maxOutput, defaultCwd: defaultCwd}
}

type execCommand struct {
	ex         Executor
	gate       *approval.Gate
	maxOutput  int64
	defaultCwd string
}

func (t *execCommand) Definition() llm.ToolDefinition {
	return llm.ToolDefinition{
		Name:        NameExecuteCommand,
		Description: "Run a shell command on the target machine and return its exit code, stdout, and stderr. Requires user approval unless approval is allow-all.",
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
	var p struct {
		Command string `json:"command"`
		Cwd     string `json:"cwd"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return "", fmt.Errorf("parse arguments: %w", err)
	}

	d, err := t.gate.Check(ctx, approval.Request{
		ToolName: NameExecuteCommand,
		Command:  p.Command,
		Args:     string(args),
	})
	if err != nil {
		return "", err
	}
	if d == approval.DecisionDenied {
		return "denied by user", nil
	}

	cwd := p.Cwd
	if cwd == "" {
		cwd = t.defaultCwd
	}
	r, err := t.ex.Exec(ctx, p.Command, execution.ExecOptions{
		Cwd:            cwd,
		MaxOutputBytes: t.maxOutput,
	})
	if err != nil {
		return "", err
	}
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
