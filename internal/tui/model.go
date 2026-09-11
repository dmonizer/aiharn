// Package tui implements the terminal UI. It is a thin Bubbletea layer over the
// agent runtime: the manager, focused agent, and approval gate are injected,
// their events are bridged into tea messages, and the model renders a focused
// transcript, a subagent list, and an input line. It holds no business logic.
package tui

import (
	"context"
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"aiharn/internal/agent"
	"aiharn/internal/approval"
	"aiharn/internal/tools"
)

// Status is the immutable startup-banner content shown in the status bar.
type Status struct {
	Model     string // model config name
	AgentType string
	Channel   string
	Approval  string // initial approval mode
}

// Model is the Bubbletea root model.
type Model struct {
	manager *agent.Manager
	agent   *agent.Agent // the focused (top-level) agent
	gate    *approval.Gate
	status  Status

	lines   []string // flushed transcript lines, oldest first
	curText string   // streamed text not yet flushed to a line
	input   string   // current input buffer
	queue   []string // inputs waiting for the agent to become idle
	running bool

	subagents []tools.SubagentStatus // current roster snapshot

	pending  *approval.Request // active approval modal, nil when none
	quitting bool

	ctx    context.Context
	cancel context.CancelFunc

	width  int
	height int
}

// New returns a Model driving the top-level agent under the given manager and
// approval gate. ctx is cancelled on quit to abort any in-flight turn.
func New(mgr *agent.Manager, top *agent.Agent, g *approval.Gate, status Status) *Model {
	ctx, cancel := context.WithCancel(context.Background())
	m := &Model{
		manager: mgr,
		agent:   top,
		gate:    g,
		status:  status,
		ctx:     ctx,
		cancel:  cancel,
	}
	m.appendLine(fmt.Sprintf("aiharn: agent %s · model %s · channel %s · approval %s",
		status.AgentType, status.Model, status.Channel, status.Approval))
	m.refreshSubagents()
	return m
}

// Init starts the agent-event, approval, and roster bridges.
func (m *Model) Init() tea.Cmd {
	return tea.Batch(waitAgentEvent(m.agent), waitApproval(m.gate), waitRoster(m.manager))
}

// refreshSubagents re-reads the subagent roster from the manager.
func (m *Model) refreshSubagents() {
	if m.manager == nil {
		m.subagents = nil
		return
	}
	subs, err := m.manager.ListSubagents(m.ctx, m.agent.ID())
	if err != nil {
		m.subagents = nil
		return
	}
	m.subagents = subs
}

// appendLine appends a completed transcript line.
func (m *Model) appendLine(s string) {
	m.flushText()
	m.lines = append(m.lines, s)
}

// appendText accumulates a streamed text delta, flushing complete lines.
func (m *Model) appendText(s string) {
	m.curText += s
	for {
		i := strings.IndexByte(m.curText, '\n')
		if i < 0 {
			return
		}
		m.lines = append(m.lines, m.curText[:i])
		m.curText = m.curText[i+1:]
	}
}

// flushText moves any partial streamed text into the transcript.
func (m *Model) flushText() {
	if m.curText != "" {
		m.lines = append(m.lines, m.curText)
		m.curText = ""
	}
}
