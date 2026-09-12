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
		return m, waitAgentEventContext(m.ctx, m.agent)

	case approvalReqMsg:
		if m.pending == nil {
			m.pending = &msg.req
		} else {
			m.approvals = append(m.approvals, msg.req)
		}
		m.appendLine(fmt.Sprintf("[approval] %s %s", msg.req.ToolName, msg.req.Command))
		return m, waitApprovalContext(m.ctx, m.gate)

	case rosterMsg:
		m.refreshSubagents()
		return m, waitRosterContext(m.ctx, m.manager)

	case bridgeStoppedMsg:
		return m, nil

	case turnDoneMsg:
		m.flushText()
		m.running = false
		if msg.err != nil {
			m.appendLine("error: " + msg.err.Error())
		}
		m.refreshSubagents()
		if cmd := m.nextTurn(); cmd != nil {
			return m, cmd
		}
		return m, nil
	}
	return m, nil
}

func (m *Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if msg.String() == "ctrl+c" {
		m.cancel()
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
	var err error
	switch msg.String() {
	case "y":
		err = m.gate.Decide(req.ID, approval.DecisionApproved)
	case "n":
		err = m.gate.Decide(req.ID, approval.DecisionDenied)
	case "a":
		m.gate.SetMode(approval.ModeAllowAll)
		err = m.gate.Decide(req.ID, approval.DecisionApproved)
	default:
		return nil
	}
	if err != nil {
		m.appendLine("approval error: " + err.Error())
	}
	if len(m.approvals) == 0 {
		m.pending = nil
	} else {
		next := m.approvals[0]
		m.approvals = m.approvals[1:]
		m.pending = &next
	}
	return nil
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
	return runTurn(m.agent, m.ctx, input)
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
