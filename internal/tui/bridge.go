package tui

import (
	"context"

	tea "github.com/charmbracelet/bubbletea"

	"aiharn/internal/agent"
	"aiharn/internal/approval"
)

// Message types bridging agent/gate activity into the Bubbletea event loop.
// They are unexported; Update switches on them.
type (
	agentEventMsg  struct{ ev agent.Event }
	approvalReqMsg struct{ req approval.Request }
	turnDoneMsg    struct{ err error }
)

// waitAgentEvent blocks until the agent emits an event and delivers it as a
// message. The model re-subscribes after each event, so the bridge never blocks
// Update: the goroutine waits on the channel, and delivery is just a message
// enqueue.
func waitAgentEvent(a *agent.Agent) tea.Cmd {
	return func() tea.Msg { return agentEventMsg{ev: <-a.Events()} }
}

// waitApproval blocks until the gate publishes a pending request.
func waitApproval(g *approval.Gate) tea.Cmd {
	return func() tea.Msg { return approvalReqMsg{req: <-g.Pending()} }
}

// runTurn runs one agent turn in a goroutine and reports completion. It uses a
// background context: Phase 7's Manager supplies cancellation.
func runTurn(a *agent.Agent, input string) tea.Cmd {
	return func() tea.Msg {
		return turnDoneMsg{err: a.Turn(context.Background(), input)}
	}
}
