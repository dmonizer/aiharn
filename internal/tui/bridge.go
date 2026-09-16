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
	agentEventMsg    struct{ ev agent.Event }
	approvalReqMsg   struct{ req approval.Request }
	turnDoneMsg      struct{ err error }
	rosterMsg        struct{}
	bridgeStoppedMsg struct{}
	thinkingTickMsg  struct{}
)

func tickThinking() tea.Cmd {
	return tea.Tick(time.Second, func(time.Time) tea.Msg { return thinkingTickMsg{} })
}

// waitAgentEvent blocks until the agent emits an event and delivers it as a
// message. The model re-subscribes after each event, so the bridge never blocks
// Update: the goroutine waits on the channel, and delivery is just a message
// enqueue.
func waitAgentEventContext(ctx context.Context, a *agent.Agent) tea.Cmd {
	return func() tea.Msg {
		select {
		case ev := <-a.Events():
			return agentEventMsg{ev: ev}
		case <-ctx.Done():
			return bridgeStoppedMsg{}
		}
	}
}

// waitApproval blocks until the gate publishes a pending request.
func waitApprovalContext(ctx context.Context, g *approval.Gate) tea.Cmd {
	return func() tea.Msg {
		select {
		case req := <-g.Pending():
			return approvalReqMsg{req: req}
		case <-ctx.Done():
			return bridgeStoppedMsg{}
		}
	}
}

// waitRoster blocks until the manager pings its roster channel.
func waitRosterContext(ctx context.Context, m *agent.Manager) tea.Cmd {
	return func() tea.Msg {
		select {
		case <-m.Roster():
			return rosterMsg{}
		case <-ctx.Done():
			return bridgeStoppedMsg{}
		}
	}
}

// runTurn runs one agent turn in a goroutine and reports completion. It uses the
// model's cancellable context so quitting aborts an in-flight turn.
func runTurn(a *agent.Agent, ctx context.Context, input string) tea.Cmd {
	return func() tea.Msg {
		return turnDoneMsg{err: a.Turn(ctx, input)}
	}
}
