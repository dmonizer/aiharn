// Package tui implements the terminal UI. It is a thin Bubbletea layer over the
// agent runtime: the agent and approval gate are injected, their events are
// bridged into tea messages, and the model renders a transcript plus an input
// line. It holds no business logic.
package tui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"aiharn/internal/agent"
	"aiharn/internal/approval"
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
	agent  *agent.Agent
	gate   *approval.Gate
	status Status

	lines   []string // flushed transcript lines, oldest first
	curText string   // streamed text not yet flushed to a line
	input   string   // current input buffer
	queue   []string // inputs waiting for the agent to become idle
	running bool

	pending  *approval.Request // active approval modal, nil when none
	quitting bool

	width  int
	height int
}

// New returns a Model driving the given agent under the given approval gate.
func New(a *agent.Agent, g *approval.Gate, status Status) *Model {
	m := &Model{
		agent:  a,
		gate:   g,
		status: status,
	}
	m.appendLine(fmt.Sprintf("aiharn: agent %s · model %s · channel %s · approval %s",
		status.AgentType, status.Model, status.Channel, status.Approval))
	return m
}

// Init starts the agent-event and approval bridges.
func (m *Model) Init() tea.Cmd {
	return tea.Batch(waitAgentEvent(m.agent), waitApproval(m.gate))
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
