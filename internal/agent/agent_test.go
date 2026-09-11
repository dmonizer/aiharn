package agent_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"aiharn/internal/agent"
	"aiharn/internal/approval"
	"aiharn/internal/execution"
	"aiharn/internal/llm"
	"aiharn/internal/tools"
	testexec "aiharn/internal/testutil/execution"
	testllm "aiharn/internal/testutil/llm"
)

func drainEvents(a *agent.Agent) []agent.Event {
	var out []agent.Event
	for {
		select {
		case e := <-a.Events():
			out = append(out, e)
		default:
			return out
		}
	}
}

func TestTurnSimple(t *testing.T) {
	client := &testllm.FakeClient{
		Script: [][]llm.Event{
			{
				{Type: llm.EventTextDelta, Text: "Hel"},
				{Type: llm.EventTextDelta, Text: "lo"},
				{Type: llm.EventCompleted, Items: []llm.Item{
					{Type: llm.ItemMessage, Role: llm.RoleAssistant, Content: "Hello"},
				}},
			},
		},
	}
	a := agent.New(agent.Spec{ID: "a1", Type: "main", Model: "m", System: "sys", Client: client})

	if err := a.Turn(context.Background(), "hi"); err != nil {
		t.Fatalf("Turn: %v", err)
	}
	if a.State() != agent.StateIdle {
		t.Fatalf("state = %v", a.State())
	}

	hist := a.History()
	if len(hist) != 2 {
		t.Fatalf("history len = %d: %+v", len(hist), hist)
	}
	if hist[0].Type != llm.ItemMessage || hist[0].Role != llm.RoleUser || hist[0].Content != "hi" {
		t.Fatalf("history[0] = %+v", hist[0])
	}
	if hist[1].Content != "Hello" {
		t.Fatalf("history[1] = %+v", hist[1])
	}

	events := drainEvents(a)
	if !hasText(events, "Hel") || !hasText(events, "lo") {
		t.Fatalf("missing streamed text: %+v", events)
	}

	reqs := client.Requests()
	if len(reqs) != 1 {
		t.Fatalf("stream calls = %d", len(reqs))
	}
	if reqs[0].System != "sys" || reqs[0].Model != "m" {
		t.Fatalf("request = %+v", reqs[0])
	}
}

func TestTurnToolCall(t *testing.T) {
	client := &testllm.FakeClient{
		Script: [][]llm.Event{
			{
				{Type: llm.EventCompleted, Items: []llm.Item{
					{Type: llm.ItemFunctionCall, CallID: "c1", Name: tools.NameExecuteCommand, Args: `{"command":"pwd"}`},
				}},
			},
			{
				{Type: llm.EventCompleted, Items: []llm.Item{
					{Type: llm.ItemMessage, Role: llm.RoleAssistant, Content: "done"},
				}},
			},
		},
	}

	ex := testexec.NewSession(func(ctx context.Context, cmd string, opts execution.ExecOptions) (execution.Result, error) {
		return execution.Result{Stdout: "/home\n", ExitCode: 0}, nil
	})
	gate := approval.NewGate(approval.ModeAllowAll)
	reg := tools.New()
	if err := reg.Register(tools.ExecuteCommand(ex, gate, 0)); err != nil {
		t.Fatal(err)
	}

	a := agent.New(agent.Spec{ID: "a1", Type: "main", Model: "m", System: "sys", Client: client, Tools: reg})
	if err := a.Turn(context.Background(), "run pwd"); err != nil {
		t.Fatalf("Turn: %v", err)
	}

	if got := len(ex.Calls()); got != 1 {
		t.Fatalf("executor calls = %d", got)
	}
	if ex.Calls()[0].Cmd != "pwd" {
		t.Fatalf("cmd = %q", ex.Calls()[0].Cmd)
	}

	hist := a.History()
	if len(hist) != 4 {
		t.Fatalf("history len = %d: %+v", len(hist), hist)
	}
	if hist[1].Type != llm.ItemFunctionCall {
		t.Fatalf("history[1] = %+v", hist[1])
	}
	if hist[2].Type != llm.ItemFunctionCallOutput || hist[2].CallID != "c1" {
		t.Fatalf("history[2] = %+v", hist[2])
	}
	if !strings.Contains(hist[2].Content, "/home") {
		t.Fatalf("tool output = %q", hist[2].Content)
	}
	if hist[3].Content != "done" {
		t.Fatalf("history[3] = %+v", hist[3])
	}

	if got := len(client.Requests()); got != 2 {
		t.Fatalf("stream calls = %d", got)
	}
	if len(client.Requests()[1].Tools) != 1 {
		t.Fatalf("second request tools = %+v", client.Requests()[1].Tools)
	}

	events := drainEvents(a)
	if !hasToolCall(events, tools.NameExecuteCommand) {
		t.Fatalf("missing tool-call event: %+v", events)
	}
}

func TestTurnSetupError(t *testing.T) {
	client := &testllm.FakeClient{SetupErr: errors.New("no auth")}
	a := agent.New(agent.Spec{ID: "a1", Type: "main", Model: "m", System: "sys", Client: client})

	if err := a.Turn(context.Background(), "hi"); err == nil {
		t.Fatal("expected error")
	}
	if a.State() != agent.StateErrored {
		t.Fatalf("state = %v", a.State())
	}
}

func TestTurnFailedEvent(t *testing.T) {
	client := &testllm.FakeClient{
		Script: [][]llm.Event{
			{{Type: llm.EventFailed, Err: errors.New("boom")}},
		},
	}
	a := agent.New(agent.Spec{ID: "a1", Type: "main", Model: "m", System: "sys", Client: client})

	if err := a.Turn(context.Background(), "hi"); err == nil {
		t.Fatal("expected error")
	}
	if a.State() != agent.StateErrored {
		t.Fatalf("state = %v", a.State())
	}
}

func TestTurnNoTerminalEvent(t *testing.T) {
	client := &testllm.FakeClient{Script: [][]llm.Event{{{Type: llm.EventTextDelta, Text: "x"}}}}
	a := agent.New(agent.Spec{ID: "a1", Type: "main", Model: "m", System: "sys", Client: client})

	if err := a.Turn(context.Background(), "hi"); err == nil {
		t.Fatal("expected error for stream without terminal event")
	}
}

func hasText(events []agent.Event, text string) bool {
	for _, e := range events {
		if e.Type == agent.EventText && e.Text == text {
			return true
		}
	}
	return false
}

func hasToolCall(events []agent.Event, name string) bool {
	for _, e := range events {
		if e.Type == agent.EventToolCall && e.Call.Name == name {
			return true
		}
	}
	return false
}
