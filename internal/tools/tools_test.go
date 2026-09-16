package tools_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"aiharn/internal/approval"
	"aiharn/internal/execution"
	testexec "aiharn/internal/testutil/execution"
	"aiharn/internal/tools"
)

func newRegistry(t *testing.T, tts ...tools.Tool) *tools.Registry {
	t.Helper()
	r := tools.New()
	for _, tool := range tts {
		if err := r.Register(tool); err != nil {
			t.Fatalf("Register: %v", err)
		}
	}
	return r
}

func echoExecutor(t *testing.T) *testexec.Session {
	t.Helper()
	return testexec.NewSession(func(ctx context.Context, cmd string, opts execution.ExecOptions) (execution.Result, error) {
		return execution.Result{Stdout: "output of " + cmd + "\n", ExitCode: 0}, nil
	})
}

func TestExecuteCommandAllowAll(t *testing.T) {
	ex := echoExecutor(t)
	gate := approval.NewGate(approval.ModeAllowAll)
	r := newRegistry(t, tools.ExecuteCommand(ex, gate, 0, "", 0, "agent-1", "main"))

	out, err := r.Run(context.Background(), tools.NameExecuteCommand, json.RawMessage(`{"command":"echo hi"}`))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(out, "exit_code: 0") {
		t.Fatalf("missing exit code: %q", out)
	}
	if !strings.Contains(out, "output of echo hi") {
		t.Fatalf("missing stdout: %q", out)
	}
}

func TestExecuteCommandDefaultCwd(t *testing.T) {
	ex := testexec.NewSession(nil)
	gate := approval.NewGate(approval.ModeAllowAll)
	r := newRegistry(t, tools.ExecuteCommand(ex, gate, 0, "/srv/work", 0, "agent-1", "main"))

	// No cwd in args → the default applies.
	if _, err := r.Run(context.Background(), tools.NameExecuteCommand, json.RawMessage(`{"command":"pwd"}`)); err != nil {
		t.Fatal(err)
	}
	// An explicit cwd overrides the default.
	if _, err := r.Run(context.Background(), tools.NameExecuteCommand, json.RawMessage(`{"command":"pwd","cwd":"/tmp"}`)); err != nil {
		t.Fatal(err)
	}

	calls := ex.Calls()
	if len(calls) != 2 {
		t.Fatalf("calls = %d, want 2", len(calls))
	}
	if calls[0].Opts.Cwd != "/srv/work" {
		t.Fatalf("default cwd = %q, want /srv/work", calls[0].Opts.Cwd)
	}
	if calls[1].Opts.Cwd != "/tmp" {
		t.Fatalf("explicit cwd = %q, want /tmp", calls[1].Opts.Cwd)
	}
}

func TestExecuteCommandAskApproved(t *testing.T) {
	ex := echoExecutor(t)
	gate := approval.NewGate(approval.ModeAsk)
	r := newRegistry(t, tools.ExecuteCommand(ex, gate, 0, "", 0, "agent-1", "main"))

	done := make(chan string, 1)
	go func() {
		out, _ := r.Run(context.Background(), tools.NameExecuteCommand, json.RawMessage(`{"command":"ls"}`))
		done <- out
	}()

	var req approval.Request
	select {
	case req = <-gate.Pending():
	case <-time.After(time.Second):
		t.Fatal("no approval request surfaced")
	}
	if req.ToolName != tools.NameExecuteCommand {
		t.Fatalf("tool = %q", req.ToolName)
	}
	if req.AgentID != "agent-1" || req.AgentType != "main" {
		t.Fatalf("agent = %q (%q)", req.AgentType, req.AgentID)
	}
	if err := gate.Decide(req.ID, approval.DecisionApproved); err != nil {
		t.Fatal(err)
	}

	select {
	case out := <-done:
		if !strings.Contains(out, "output of ls") {
			t.Fatalf("unexpected output: %q", out)
		}
	case <-time.After(time.Second):
		t.Fatal("tool did not return after approval")
	}
}

func TestExecuteCommandAskDenied(t *testing.T) {
	ex := echoExecutor(t)
	gate := approval.NewGate(approval.ModeAsk)
	r := newRegistry(t, tools.ExecuteCommand(ex, gate, 0, "", 0, "agent-1", "main"))

	done := make(chan string, 1)
	go func() {
		out, _ := r.Run(context.Background(), tools.NameExecuteCommand, json.RawMessage(`{"command":"rm -rf /"}`))
		done <- out
	}()

	var req approval.Request
	select {
	case req = <-gate.Pending():
	case <-time.After(time.Second):
		t.Fatal("no approval request surfaced")
	}
	if err := gate.Decide(req.ID, approval.DecisionDenied); err != nil {
		t.Fatal(err)
	}

	select {
	case out := <-done:
		if out != "denied by user" {
			t.Fatalf("got %q", out)
		}
	case <-time.After(time.Second):
		t.Fatal("tool did not return after denial")
	}

	if got := len(ex.Calls()); got != 0 {
		t.Fatalf("command ran despite denial (%d calls)", got)
	}
}

func TestExecuteCommandInvalidArgs(t *testing.T) {
	ex := echoExecutor(t)
	gate := approval.NewGate(approval.ModeAllowAll)
	r := newRegistry(t, tools.ExecuteCommand(ex, gate, 0, "", 0, "agent-1", "main"))

	_, err := r.Run(context.Background(), tools.NameExecuteCommand, json.RawMessage(`{}`))
	if err == nil || !strings.Contains(err.Error(), "invalid arguments") {
		t.Fatalf("expected invalid-arguments error, got %v", err)
	}
}

func TestExecuteCommandMalformedJSON(t *testing.T) {
	ex := echoExecutor(t)
	gate := approval.NewGate(approval.ModeAllowAll)
	r := newRegistry(t, tools.ExecuteCommand(ex, gate, 0, "", 0, "agent-1", "main"))

	_, err := r.Run(context.Background(), tools.NameExecuteCommand, json.RawMessage(`{not json`))
	if err == nil {
		t.Fatal("expected error for malformed args")
	}
}

func TestExecuteCommandRejectsTrailingJSON(t *testing.T) {
	ex := echoExecutor(t)
	gate := approval.NewGate(approval.ModeAllowAll)
	r := newRegistry(t, tools.ExecuteCommand(ex, gate, 0, "", 0, "agent-1", "main"))

	_, err := r.Run(context.Background(), tools.NameExecuteCommand, json.RawMessage(`{"command":"pwd"} {}`))
	if err == nil || !strings.Contains(err.Error(), "multiple JSON values") {
		t.Fatalf("err = %v", err)
	}
}

func TestExecuteCommandTimeout(t *testing.T) {
	ex := testexec.NewSession(func(ctx context.Context, cmd string, opts execution.ExecOptions) (execution.Result, error) {
		<-ctx.Done()
		return execution.Result{}, ctx.Err()
	})
	gate := approval.NewGate(approval.ModeAllowAll)
	r := newRegistry(t, tools.ExecuteCommand(ex, gate, 0, "", 20*time.Millisecond, "agent-1", "main"))

	_, err := r.Run(context.Background(), tools.NameExecuteCommand, json.RawMessage(`{"command":"sleep"}`))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want DeadlineExceeded", err)
	}
}

func TestSetApprovalTighten(t *testing.T) {
	gate := approval.NewGate(approval.ModeAllowAll)
	r := newRegistry(t, tools.SetApproval(gate))

	out, err := r.Run(context.Background(), tools.NameSetApproval, json.RawMessage(`{"mode":"ask"}`))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if gate.Mode() != approval.ModeAsk {
		t.Fatalf("mode = %v", gate.Mode())
	}
	if !strings.Contains(out, "ask") {
		t.Fatalf("output = %q", out)
	}
}

func TestSetApprovalCannotLoosen(t *testing.T) {
	gate := approval.NewGate(approval.ModeAsk)
	r := newRegistry(t, tools.SetApproval(gate))

	_, err := r.Run(context.Background(), tools.NameSetApproval, json.RawMessage(`{"mode":"allow-all"}`))
	if err == nil {
		t.Fatal("expected error for loosening")
	}
	if gate.Mode() != approval.ModeAsk {
		t.Fatalf("mode changed to %v", gate.Mode())
	}
}

func TestRegistryUnknownTool(t *testing.T) {
	r := tools.New()
	if _, err := r.Run(context.Background(), "nope", json.RawMessage(`{}`)); err == nil {
		t.Fatal("expected unknown-tool error")
	}
}

func TestRegistryDefinitionsOrder(t *testing.T) {
	ex := echoExecutor(t)
	gate := approval.NewGate(approval.ModeAllowAll)
	r := newRegistry(t, tools.ExecuteCommand(ex, gate, 0, "", 0, "agent-1", "main"), tools.SetApproval(gate))

	defs := r.Definitions()
	if len(defs) != 2 {
		t.Fatalf("got %d definitions", len(defs))
	}
	if defs[0].Name != tools.NameExecuteCommand || defs[1].Name != tools.NameSetApproval {
		t.Fatalf("order = %v", r.Names())
	}
}
