package tui

import (
	"context"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"aiharn/internal/agent"
	"aiharn/internal/approval"
)

// Message types bridging agent/gate/manager activity into the Bubbletea event
// loop. They are unexported; Update switches on them.
type (
	agentEventMsg struct {
		gen uint64
		ev  agent.Event
	}
	approvalReqMsg struct {
		gen uint64
		req approval.Request
	}
	approvalResolvedMsg struct {
		gen uint64
		id  string
	}
	turnDoneMsg struct {
		gen uint64
		err error
	}
	rosterMsg         struct{ gen uint64 }
	bridgeStoppedMsg  struct{ gen uint64 }
	thinkingTickMsg   struct{}
	subagentClosedMsg struct {
		gen uint64
		id  string
		err error
	}
	clearDoneMsg struct {
		gen    uint64
		result ClearResult
		err    error
	}
)

func tickThinking() tea.Cmd {
	return tea.Tick(time.Second, func(time.Time) tea.Msg { return thinkingTickMsg{} })
}

// waitAgentEvent blocks until the agent emits an event and delivers it as a
// message. The model re-subscribes after each event, so the bridge never blocks
// Update: the goroutine waits on the channel, and delivery is just a message
// enqueue.
func waitAgentEventContext(ctx context.Context, a *agent.Agent, gen uint64) tea.Cmd {
	return func() tea.Msg {
		select {
		case ev := <-a.Events():
			return agentEventMsg{gen: gen, ev: ev}
		case <-ctx.Done():
			return bridgeStoppedMsg{gen: gen}
		}
	}
}

// waitApproval blocks until the gate publishes a pending request.
func waitApprovalContext(ctx context.Context, g *approval.Gate, gen uint64) tea.Cmd {
	return func() tea.Msg {
		select {
		case req := <-g.Pending():
			return approvalReqMsg{gen: gen, req: req}
		case <-ctx.Done():
			return bridgeStoppedMsg{gen: gen}
		}
	}
}

// waitApprovalResolved blocks until the gate reports a request was resolved by
// any client. The TUI uses it to clear a prompt the web console has already
// decided, so a stale prompt can never stay active across the two UIs.
func waitApprovalResolvedContext(ctx context.Context, g *approval.Gate, gen uint64) tea.Cmd {
	return func() tea.Msg {
		select {
		case id := <-g.Resolved():
			return approvalResolvedMsg{gen: gen, id: id}
		case <-ctx.Done():
			return bridgeStoppedMsg{gen: gen}
		}
	}
}

// waitRoster blocks until the manager pings its roster channel.
func waitRosterContext(ctx context.Context, m *agent.Manager, gen uint64) tea.Cmd {
	return func() tea.Msg {
		select {
		case <-m.Roster():
			return rosterMsg{gen: gen}
		case <-ctx.Done():
			return bridgeStoppedMsg{gen: gen}
		}
	}
}

// runTurn runs one agent turn in a goroutine and reports completion. It uses the
// model's cancellable context so quitting aborts an in-flight turn.
func runTurn(a *agent.Agent, ctx context.Context, input string, gen uint64) tea.Cmd {
	return func() tea.Msg {
		return turnDoneMsg{gen: gen, err: a.Turn(ctx, input)}
	}
}
