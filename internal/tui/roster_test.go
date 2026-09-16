package tui

import (
	"context"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

	"aiharn/internal/agent"
	"aiharn/internal/approval"
	"aiharn/internal/llm"
	testllm "aiharn/internal/testutil/llm"
)

// newRosterModel builds a model whose top-level agent may spawn subagents and
// whose manager materializes subagents that complete one scripted turn.
func newRosterModel(t *testing.T) *Model {
	t.Helper()
	top := agent.New(agent.Spec{
		ID:             "main",
		Type:           "main",
		AllowSubagents: true,
		Client:         &testllm.FakeClient{},
	})
	mgr := agent.NewManager(agent.ManagerOptions{
		Builder: func(ctx context.Context, spec agent.SpawnSpec) (*agent.Agent, error) {
			return agent.New(agent.Spec{
				ID: spec.ID, Type: spec.Type, Depth: spec.Depth, CallerID: spec.CallerID,
				Client: &testllm.FakeClient{Script: [][]llm.Event{
					{{Type: llm.EventCompleted, Items: []llm.Item{{Type: llm.ItemMessage, Role: llm.RoleAssistant, Content: "done"}}}},
					{{Type: llm.EventCompleted, Items: []llm.Item{{Type: llm.ItemMessage, Role: llm.RoleAssistant, Content: "done again"}}}},
				}},
			}), nil
		},
	})
	if err := mgr.RegisterTop(top); err != nil {
		t.Fatal(err)
	}
	g := approval.NewGate(approval.ModeAsk)
	return New(mgr, top, g, Status{Model: "m", AgentType: "main", Channel: "devbox", Approval: "ask"})
}

func TestRefreshSubagentsReflectsManager(t *testing.T) {
	m := newRosterModel(t)
	if len(m.subagents) != 0 {
		t.Fatalf("initial subagents = %v", m.subagents)
	}

	id, err := m.manager.SpawnSubagent(context.Background(), "main", "coder", "task")
	if err != nil {
		t.Fatal(err)
	}
	m.refreshSubagents()

	if len(m.subagents) != 1 || m.subagents[0].ID != id {
		t.Fatalf("subagents = %+v, want [%s]", m.subagents, id)
	}
}

func TestRosterMsgRefreshes(t *testing.T) {
	m := newRosterModel(t)
	if _, err := m.manager.SpawnSubagent(context.Background(), "main", "coder", "task"); err != nil {
		t.Fatal(err)
	}

	m, cmd := upd(t, m, rosterMsg{})
	if len(m.subagents) != 1 {
		t.Fatalf("subagents = %+v after roster msg", m.subagents)
	}
	if cmd == nil {
		t.Fatal("expected re-subscription command")
	}
}

func TestViewListsSubagents(t *testing.T) {
	m := newRosterModel(t)
	id, err := m.manager.SpawnSubagent(context.Background(), "main", "coder", "task")
	if err != nil {
		t.Fatal(err)
	}
	m.refreshSubagents()
	m.width = 120
	m.height = 30

	v := m.View()
	if !strings.Contains(v, "subagents (1)") {
		t.Fatalf("view missing roster header:\n%s", v)
	}
	if !strings.Contains(v, id) {
		t.Fatalf("view missing subagent id %q:\n%s", id, v)
	}
}

func TestRosterHoverFocusPauseCloseAndSeparateContexts(t *testing.T) {
	m := newRosterModel(t)
	id, err := m.manager.SpawnSubagent(context.Background(), "main", "coder", "task")
	if err != nil {
		t.Fatal(err)
	}
	m.refreshSubagents()
	m.width, m.height = 120, 24
	m.resizeInput()
	m.View()
	var focus, pause, closeHit, root rosterHit
	for _, hit := range m.rosterHits {
		if hit.id == id {
			switch hit.action {
			case rosterFocus:
				focus = hit
			case rosterPause:
				pause = hit
			case rosterClose:
				closeHit = hit
			}
		} else if hit.id == "main" {
			root = hit
		}
	}
	if focus.width == 0 || pause.width == 0 || closeHit.width == 0 || root.width == 0 {
		t.Fatalf("missing roster links: %+v", m.rosterHits)
	}
	m, _ = upd(t, m, tea.MouseMsg{X: focus.x, Y: focus.y, Action: tea.MouseActionMotion})
	if m.hoverX != focus.x || m.hoverY != focus.y {
		t.Fatal("hover did not track link")
	}
	lipgloss.SetColorProfile(termenv.ANSI)
	t.Cleanup(func() { lipgloss.SetColorProfile(termenv.ANSI256) })
	if view := m.View(); !strings.Contains(view, "\x1b[4;") {
		t.Fatalf("hovered roster name was not underlined: %q", view)
	}
	lipgloss.SetColorProfile(termenv.ANSI256)
	m, _ = upd(t, m, tea.MouseMsg{X: focus.x, Y: focus.y, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress})
	if m.focusedID != id {
		t.Fatalf("focused = %q, want %q", m.focusedID, id)
	}
	m.appendAgentEvent(agent.Event{AgentID: id, Type: agent.EventText, Text: "subagent-only answer"})
	m.appendAgentEvent(agent.Event{AgentID: id, Type: agent.EventToolCall, Call: llm.Item{CallID: "sub-1", Name: "execute_command", Args: `{"command":"pwd"}`}})
	if len(m.shellCmds) != 1 {
		t.Fatalf("subagent commands = %+v", m.shellCmds)
	}
	m.View()
	m, _ = upd(t, m, tea.MouseMsg{X: root.x, Y: root.y, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress})
	if m.focusedID != "main" || len(m.shellCmds) != 0 || strings.Contains(string(m.curText), "subagent-only") {
		t.Fatalf("root view leaked subagent content: focus=%q cmds=%+v partial=%q", m.focusedID, m.shellCmds, m.curText)
	}
	m.View()
	m, _ = upd(t, m, tea.MouseMsg{X: pause.x, Y: pause.y, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress})
	if !m.manager.Agent(id).Paused() {
		t.Fatal("pause button did not pause subagent")
	}
	m, _ = upd(t, m, tea.MouseMsg{X: pause.x, Y: pause.y, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress})
	if m.manager.Agent(id).Paused() {
		t.Fatal("pause button did not resume subagent")
	}
	m, cmd := upd(t, m, tea.MouseMsg{X: closeHit.x, Y: closeHit.y, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress})
	if cmd == nil {
		t.Fatal("close button did not dispatch a close command")
	}
	msg := runCmd(cmd)
	m, _ = upd(t, m, msg)
	if m.manager.Agent(id).State() != agent.StateClosed {
		t.Fatal("close button did not close subagent")
	}
	m.focusAgent(id)
	foundAnswer := false
	for _, ln := range m.lines {
		if strings.Contains(ln.text, "subagent-only") {
			foundAnswer = true
		}
	}
	if len(m.shellCmds) != 1 || !foundAnswer {
		t.Fatalf("subagent context lost after switching: cmds=%+v lines=%+v", m.shellCmds, m.lines)
	}
}

func TestRootTurnCompletionDoesNotFlushFocusedSubagent(t *testing.T) {
	m := newRosterModel(t)
	id, err := m.manager.SpawnSubagent(context.Background(), "main", "coder", "task")
	if err != nil {
		t.Fatal(err)
	}
	m.focusAgent(id)
	m.appendAgentEvent(agent.Event{AgentID: id, Type: agent.EventText, Text: "sub still streaming"})
	m.appendAgentEvent(agent.Event{AgentID: "main", Type: agent.EventText, Text: "root final"})
	m.running = true
	m, _ = upd(t, m, turnDoneMsg{})
	if m.focusedID != id || string(m.curText) != "sub still streaming" {
		t.Fatalf("root completion changed subagent view: focus=%q text=%q", m.focusedID, m.curText)
	}
	m.focusAgent("main")
	if len(m.curText) != 0 || len(m.lines) == 0 || m.lines[len(m.lines)-1].text != "root final" {
		t.Fatalf("root result missing after background completion: lines=%+v partial=%q", m.lines, m.curText)
	}
}

func TestInputInSubagentViewQueuesToSelectedAgent(t *testing.T) {
	m := newRosterModel(t)
	id, err := m.manager.SpawnSubagent(context.Background(), "main", "coder", "first")
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for m.manager.Agent(id).State() != agent.StateIdle && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if m.manager.Agent(id).State() != agent.StateIdle {
		t.Fatal("initial subagent turn did not finish")
	}
	m.focusAgent(id)
	m, _ = upd(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("follow up")})
	m, cmd := upd(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if cmd != nil || len(m.queue) != 0 {
		t.Fatalf("subagent submission started root turn: cmd=%v queue=%v", cmd != nil, m.queue)
	}
	deadline = time.Now().Add(time.Second)
	for len(m.manager.Agent(id).History()) < 4 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	history := m.manager.Agent(id).History()
	if len(history) != 4 || history[2].Content != "follow up" {
		t.Fatalf("selected subagent did not receive prompt: %+v", history)
	}
	if got := m.agent.History(); len(got) != 0 {
		t.Fatalf("root received subagent prompt: %+v", got)
	}
}

func TestQuitCancelsTurnContext(t *testing.T) {
	m := newRosterModel(t)
	if m.ctx.Err() != nil {
		t.Fatal("ctx already cancelled")
	}
	_, _ = upd(t, m, tea.KeyMsg{Type: tea.KeyCtrlC})
	if m.ctx.Err() == nil {
		t.Fatal("ctx not cancelled on quit")
	}
}
