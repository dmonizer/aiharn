package tui

import (
	"context"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"aiharn/internal/agent"
	"aiharn/internal/approval"
	"aiharn/internal/llm"
	testllm "aiharn/internal/testutil/llm"
)

func newTestModel(t *testing.T) *Model {
	t.Helper()
	client := &testllm.FakeClient{}
	a := agent.New(agent.Spec{ID: "a1", Type: "main", Model: "m", System: "s", Client: client})
	mgr := agent.NewManager(agent.ManagerOptions{Builder: func(ctx context.Context, spec agent.SpawnSpec) (*agent.Agent, error) {
		return agent.New(agent.Spec{ID: spec.ID, Type: spec.Type, Client: &testllm.FakeClient{}}), nil
	}})
	if err := mgr.RegisterTop(a); err != nil {
		t.Fatal(err)
	}
	g := approval.NewGate(approval.ModeAsk)
	return New(mgr, a, g, Status{Model: "m", AgentType: "main", Channel: "devbox", Approval: "ask"})
}

// upd runs m.Update and returns the concrete model and command.
func upd(t *testing.T, m *Model, msg tea.Msg) (*Model, tea.Cmd) {
	t.Helper()
	nm, cmd := m.Update(msg)
	return nm.(*Model), cmd
}

// runCmd runs a returned tea.Cmd synchronously and returns its message, or nil.
func runCmd(cmd tea.Cmd) tea.Msg {
	if cmd == nil {
		return nil
	}
	return cmd()
}

func TestUpdateTextDelta(t *testing.T) {
	m := newTestModel(t)

	m, _ = upd(t, m, agentEventMsg{ev: agent.Event{Type: agent.EventText, Text: "Hel"}})
	m, _ = upd(t, m, agentEventMsg{ev: agent.Event{Type: agent.EventText, Text: "lo"}})

	if m.curText != "Hello" {
		t.Fatalf("curText = %q, want Hello", m.curText)
	}
}

func TestUpdateToolCallFlushesText(t *testing.T) {
	m := newTestModel(t)
	m, _ = upd(t, m, agentEventMsg{ev: agent.Event{Type: agent.EventText, Text: "run"}})
	m, _ = upd(t, m, agentEventMsg{ev: agent.Event{Type: agent.EventToolCall, Call: llm.Item{Name: "execute_command", Args: `{"command":"ls"}`}}})

	if m.curText != "" {
		t.Fatalf("curText = %q, want flushed", m.curText)
	}
	if len(m.lines) == 0 || m.lines[len(m.lines)-1] != "[tool] execute_command {\"command\":\"ls\"}" {
		t.Fatalf("last line = %q", m.lines[len(m.lines)-1])
	}
}

func TestUpdateApprovalReq(t *testing.T) {
	m := newTestModel(t)
	req := approval.Request{ID: "1", ToolName: "execute_command", Command: "rm -rf /"}
	m, cmd := upd(t, m, approvalReqMsg{req: req})

	if m.pending == nil || m.pending.Command != "rm -rf /" {
		t.Fatalf("pending = %+v", m.pending)
	}
	if cmd == nil {
		t.Fatal("expected re-subscription command")
	}
}

func TestInputQueuedAtTurnBoundary(t *testing.T) {
	m := newTestModel(t)

	// First input starts a turn immediately.
	m, _ = upd(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("one")})
	m, cmd := upd(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if !m.running {
		t.Fatal("expected running after first enter")
	}
	if cmd == nil {
		t.Fatal("expected runTurn command")
	}

	// Second input is queued while running.
	m, _ = upd(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("two")})
	m, cmd = upd(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if !m.running {
		t.Fatal("still running")
	}
	if cmd != nil {
		t.Fatal("no new turn should start while running")
	}
	if len(m.queue) != 1 || m.queue[0] != "two" {
		t.Fatalf("queue = %v", m.queue)
	}

	// Turn completion drains the queue.
	m, cmd = upd(t, m, turnDoneMsg{})
	if !m.running || cmd == nil {
		t.Fatalf("queued turn should start: running=%v cmd=%v", m.running, cmd != nil)
	}
	if len(m.queue) != 0 {
		t.Fatalf("queue = %v", m.queue)
	}

	// Second completion leaves the agent idle.
	m, cmd = upd(t, m, turnDoneMsg{})
	if m.running || cmd != nil {
		t.Fatalf("expected idle: running=%v cmd=%v", m.running, cmd != nil)
	}
}

func TestKeyHandling(t *testing.T) {
	m := newTestModel(t)

	m, _ = upd(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("ab")})
	m, _ = upd(t, m, tea.KeyMsg{Type: tea.KeyBackspace})
	if m.input != "a" {
		t.Fatalf("backspace: input = %q", m.input)
	}
	m, _ = upd(t, m, tea.KeyMsg{Type: tea.KeyEsc})
	if m.input != "" {
		t.Fatalf("esc: input = %q", m.input)
	}
	m, _ = upd(t, m, tea.KeyMsg{Type: tea.KeySpace})
	if m.input != " " {
		t.Fatalf("space: input = %q", m.input)
	}
}

func TestQuitOnCtrlC(t *testing.T) {
	m := newTestModel(t)
	_, cmd := upd(t, m, tea.KeyMsg{Type: tea.KeyCtrlC})
	if !m.quitting {
		t.Fatal("expected quitting")
	}
	if cmd == nil {
		t.Fatal("expected quit command")
	}
}

func TestApprovalKeys(t *testing.T) {
	m := newTestModel(t)
	req := approval.Request{ID: "1", ToolName: "execute_command", Command: "ls"}
	m, _ = upd(t, m, approvalReqMsg{req: req})

	_, cmd := upd(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})
	if m.pending != nil {
		t.Fatal("expected approval cleared")
	}
	if cmd == nil {
		t.Fatal("expected re-subscription")
	}
}

func TestApprovalAcceptAndAllowAll(t *testing.T) {
	m := newTestModel(t)
	req := approval.Request{ID: "1", ToolName: "execute_command", Command: "ls"}
	m, _ = upd(t, m, approvalReqMsg{req: req})

	m, _ = upd(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("a")})
	if m.gate.Mode() != approval.ModeAllowAll {
		t.Fatalf("gate mode = %v", m.gate.Mode())
	}
	if m.pending != nil {
		t.Fatal("expected approval cleared")
	}
	if !strings.Contains(m.statusLine(), "allow-all") {
		t.Fatalf("status line = %q", m.statusLine())
	}
}

func TestWaitAgentEventDelivers(t *testing.T) {
	client := &testllm.FakeClient{Script: [][]llm.Event{
		{
			{Type: llm.EventTextDelta, Text: "x"},
			{Type: llm.EventCompleted, Items: []llm.Item{{Type: llm.ItemMessage, Role: llm.RoleAssistant, Content: "x"}}},
		},
	}}
	a := agent.New(agent.Spec{ID: "a1", Type: "main", Model: "m", System: "s", Client: client})

	go func() { _ = a.Turn(context.Background(), "hi") }()

	// The bridge delivers whatever event the agent emits; the first is the
	// running state transition, not a text delta.
	msg := runCmd(waitAgentEvent(a))
	if _, ok := msg.(agentEventMsg); !ok {
		t.Fatalf("msg = %+v, want agentEventMsg", msg)
	}
}

func TestRunTurnReportsCompletion(t *testing.T) {
	client := &testllm.FakeClient{Script: [][]llm.Event{
		{{Type: llm.EventCompleted, Items: []llm.Item{{Type: llm.ItemMessage, Role: llm.RoleAssistant, Content: "ok"}}}},
	}}
	a := agent.New(agent.Spec{ID: "a1", Type: "main", Model: "m", System: "s", Client: client})

	msg := runCmd(runTurn(a, context.Background(), "hi"))
	if done, ok := msg.(turnDoneMsg); !ok || done.err != nil {
		t.Fatalf("msg = %+v", msg)
	}
}
