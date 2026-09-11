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
				Client: &testllm.FakeClient{Script: [][]llm.Event{{
					{Type: llm.EventCompleted, Items: []llm.Item{{Type: llm.ItemMessage, Role: llm.RoleAssistant, Content: "done"}}},
				}}},
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
