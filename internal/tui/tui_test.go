package tui

import (
	"context"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

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

	if string(m.curText) != "Hello" {
		t.Fatalf("curText = %q, want Hello", m.curText)
	}
}

func TestUpdateToolCallFlushesText(t *testing.T) {
	m := newTestModel(t)
	m, _ = upd(t, m, agentEventMsg{ev: agent.Event{Type: agent.EventText, Text: "run"}})
	m, _ = upd(t, m, agentEventMsg{ev: agent.Event{Type: agent.EventToolCall, Call: llm.Item{Name: "execute_command", Args: `{"command":"ls"}`}}})

	if len(m.curText) != 0 {
		t.Fatalf("curText = %q, want flushed", m.curText)
	}
	if len(m.lines) == 0 || m.lines[len(m.lines)-1].text != "[tool] execute_command {\"command\":\"ls\"}" {
		t.Fatalf("last line = %q", m.lines[len(m.lines)-1].text)
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
	if cmd == nil {
		t.Fatal("expected quit command")
	}
}

func TestQuitCommand(t *testing.T) {
	m := New(nil, nil, nil, Status{})
	cmd := m.handleCommand("/quit")
	if cmd == nil {
		t.Fatal("expected quit command")
	}
	msg := cmd()
	if _, ok := msg.(tea.QuitMsg); !ok {
		t.Fatalf("cmd() = %T, want tea.QuitMsg", msg)
	}
	if m.ctx.Err() == nil {
		t.Fatal("expected context cancelled on quit")
	}

	// /exit is an alias.
	m = New(nil, nil, nil, Status{})
	if cmd := m.handleCommand("/exit"); cmd == nil {
		t.Fatal("expected /exit to quit too")
	}
}

func TestStyleLineColors(t *testing.T) {
	// Force the 16-color ANSI profile so Render emits color codes regardless of
	// the terminal the test runs under (CI and headless runs detect "no color").
	lipgloss.SetColorProfile(termenv.ANSI)
	t.Cleanup(func() { lipgloss.SetColorProfile(termenv.ANSI256) })

	cases := []struct {
		kind lineKind
		want string // ANSI SGR color code, "" means unstyled
	}{
		{kindPlain, ""},
		{kindAssistant, ""},
		{kindUser, "36"},
		{kindTool, "33"},
		{kindError, "31"},
		{kindApproval, "35"},
	}
	for _, c := range cases {
		got := styleLine(c.kind, "hello")
		if c.want == "" {
			if strings.Contains(got, "\x1b[") {
				t.Fatalf("kind=%d got %q, want no ANSI escape", c.kind, got)
			}
			continue
		}
		if !strings.Contains(got, c.want+"m") {
			t.Fatalf("kind=%d got %q, want color %s", c.kind, got, c.want)
		}
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
	if cmd != nil {
		t.Fatal("approval bridge was already re-subscribed when the request arrived")
	}
}

func TestApprovalRequestsAreQueued(t *testing.T) {
	m := newTestModel(t)
	m, _ = upd(t, m, approvalReqMsg{req: approval.Request{ID: "1", Command: "one"}})
	m, _ = upd(t, m, approvalReqMsg{req: approval.Request{ID: "2", Command: "two"}})
	if m.pending == nil || m.pending.ID != "1" || len(m.approvals) != 1 {
		t.Fatalf("pending=%+v queue=%+v", m.pending, m.approvals)
	}
	m, _ = upd(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})
	if m.pending == nil || m.pending.ID != "2" || len(m.approvals) != 0 {
		t.Fatalf("pending=%+v queue=%+v", m.pending, m.approvals)
	}
}

func TestTerminalControlSequencesAreRemoved(t *testing.T) {
	m := newTestModel(t)
	m.appendLine(kindPlain, "safe\x1b]52;c;clipboard\a text")
	got := m.lines[len(m.lines)-1].text
	if strings.ContainsAny(got, "\x1b\a") || got != "safe]52;c;clipboard text" {
		t.Fatalf("sanitized line = %q", got)
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
	msg := runCmd(waitAgentEventContext(context.Background(), a))
	if _, ok := msg.(agentEventMsg); !ok {
		t.Fatalf("msg = %+v, want agentEventMsg", msg)
	}
}

func TestInputPinnedToBottom(t *testing.T) {
	m := newTestModel(t)
	m.width = 120
	m.height = 10
	lines := strings.Split(m.View(), "\n")
	if len(lines) != 10 {
		t.Fatalf("view has %d lines, want 10", len(lines))
	}
	if lines[len(lines)-1] != "> " {
		t.Fatalf("last line = %q, want input prompt", lines[len(lines)-1])
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
