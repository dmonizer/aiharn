package tui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"aiharn/internal/agent"
	"aiharn/internal/approval"
)

// Update implements tea.Model. It dispatches on message type and returns the
// commands to keep the bridges and turn loop alive.
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		return m, nil

	case tea.KeyMsg:
		return m.handleKey(msg)

	case agentEventMsg:
		m.appendEvent(msg.ev)
		return m, waitAgentEvent(m.agent)

	case approvalReqMsg:
		m.pending = &msg.req
		m.appendLine(fmt.Sprintf("[approval] %s %s", msg.req.ToolName, msg.req.Command))
		return m, waitApproval(m.gate)

	case turnDoneMsg:
		m.flushText()
		m.running = false
		if msg.err != nil {
			m.appendLine("error: " + msg.err.Error())
		}
		if cmd := m.nextTurn(); cmd != nil {
			return m, cmd
		}
		return m, nil
	}
	return m, nil
}

func (m *Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if msg.String() == "ctrl+c" {
		m.quitting = true
		return m, tea.Quit
	}

	if m.pending != nil {
		return m, m.handleApprovalKey(msg)
	}

	switch msg.String() {
	case "enter":
		line := strings.TrimSpace(m.input)
		m.input = ""
		if line == "" {
			return m, nil
		}
		m.queue = append(m.queue, line)
		return m, m.nextTurn()

	case "backspace":
		m.input = truncateLastRune(m.input)
		return m, nil

	case "esc":
		m.input = ""
		return m, nil

	default:
		if isPrintable(msg) {
			m.input += msg.String()
		}
		return m, nil
	}
}

func (m *Model) handleApprovalKey(msg tea.KeyMsg) tea.Cmd {
	req := m.pending
	switch msg.String() {
	case "y":
		_ = m.gate.Decide(req.ID, approval.DecisionApproved)
	case "n":
		_ = m.gate.Decide(req.ID, approval.DecisionDenied)
	case "a":
		m.gate.SetMode(approval.ModeAllowAll)
		_ = m.gate.Decide(req.ID, approval.DecisionApproved)
	default:
		return nil
	}
	m.pending = nil
	return waitApproval(m.gate)
}

// nextTurn starts the next queued input if the agent is idle and a turn is not
// already running.
func (m *Model) nextTurn() tea.Cmd {
	if m.running || len(m.queue) == 0 {
		return nil
	}
	input := m.queue[0]
	m.queue = m.queue[1:]
	m.running = true
	m.appendLine("> " + input)
	return runTurn(m.agent, input)
}

func (m *Model) appendEvent(ev agent.Event) {
	switch ev.Type {
	case agent.EventText:
		m.appendText(ev.Text)
	case agent.EventToolCall:
		m.appendLine(fmt.Sprintf("[tool] %s %s", ev.Call.Name, ev.Call.Args))
	case agent.EventState:
		m.flushText()
	}
}
