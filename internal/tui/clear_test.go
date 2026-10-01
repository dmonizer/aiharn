package tui

import (
	"context"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"aiharn/internal/agent"
	"aiharn/internal/approval"
	testllm "aiharn/internal/testutil/llm"
)

func newClearResult() ClearResult {
	client := &testllm.FakeClient{}
	a := agent.New(agent.Spec{ID: "main", Type: "main", Model: "m", System: "s", Client: client})
	mgr := agent.NewManager(agent.ManagerOptions{Builder: func(ctx context.Context, spec agent.SpawnSpec) (*agent.Agent, error) {
		return agent.New(agent.Spec{ID: spec.ID, Type: spec.Type, Client: &testllm.FakeClient{}}), nil
	}})
	_ = mgr.RegisterTop(a)
	g := approval.NewGate(approval.ModeAsk)
	return ClearResult{Manager: mgr, Agent: a, Gate: g, Model: "m", AgentType: "main", Channel: "devbox", Approval: "ask"}
}

func TestClearCommandRebindsSession(t *testing.T) {
	m := newTestModel(t)
	called := false
	m.SetClearFunc(func(ctx context.Context) (ClearResult, error) {
		called = true
		return newClearResult(), nil
	})

	cmd := m.handleCommand("/clear")
	if cmd == nil {
		t.Fatal("/clear returned no command")
	}
	msg := runCmd(cmd)
	done, ok := msg.(clearDoneMsg)
	if !ok {
		t.Fatalf("clear command returned %T, want clearDoneMsg", msg)
	}
	if !called {
		t.Fatal("clear callback was not invoked")
	}
	oldGen := m.sessionGen
	oldAgent := m.agent
	m, _ = upd(t, m, done)
	if m.sessionGen != oldGen+1 {
		t.Fatalf("sessionGen = %d, want %d", m.sessionGen, oldGen+1)
	}
	if m.agent == oldAgent || m.agent == nil || m.manager == nil || m.gate == nil {
		t.Fatal("rebind did not replace the runtime")
	}
	if len(m.lines) == 0 || m.lines[0].text == "" {
		t.Fatal("rebind did not write a new session banner")
	}
	// A stale message from the previous session must be ignored.
	stale := agentEventMsg{gen: oldGen, ev: agent.Event{Type: agent.EventText, Text: "stale"}}
	m, _ = upd(t, m, stale)
	for _, ln := range m.lines {
		if ln.text == "stale" {
			t.Fatal("stale agent event leaked into the cleared session")
		}
	}
	_ = tea.Quit
}
