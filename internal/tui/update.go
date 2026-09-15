package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/cursor"
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
		m.resizeInput()
		return m, nil

	case tea.KeyMsg:
		return m.handleKey(msg)

	case cursor.BlinkMsg:
		var cmd tea.Cmd
		m.textarea, cmd = m.textarea.Update(msg)
		return m, cmd

	case agentEventMsg:
		m.appendEvent(msg.ev)
		return m, waitAgentEventContext(m.ctx, m.agent)

	case approvalReqMsg:
		if m.pending == nil {
			m.pending = &msg.req
		} else {
			m.approvals = append(m.approvals, msg.req)
		}
		m.appendLine(kindApproval, fmt.Sprintf("[approval] %s %s", msg.req.ToolName, msg.req.Command))
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
			m.appendLine(kindError, "error: "+msg.err.Error())
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

	// Any key other than ESC disarms a pending double-ESC clear.
	if msg.String() != "esc" {
		m.lastEsc = time.Time{}
	}

	switch msg.String() {
	case "enter":
		return m, m.submitInput()

	case "esc":
		m.pressEsc(time.Now())
		return m, nil
	}

	// Everything else is editing input: printable runes (typing or bracketed
	// paste, newlines preserved) and the cursor/edit/scroll keys all go to the
	// textarea, which soft-wraps, grows to fit, and keeps the cursor visible.
	var cmd tea.Cmd
	m.textarea, cmd = m.textarea.Update(msg)
	m.resizeInput()
	// The textarea repositions its viewport inside Update, but its line buffer is
	// only rebuilt in View, so a big paste lands with a stale (empty) buffer and
	// scroll clamps to the top. Refresh the buffer and run a no-op Update so the
	// reposition pass scrolls to the cursor immediately.
	m.textarea.View()
	m.textarea, _ = m.textarea.Update(tea.KeyMsg{Type: tea.KeyRunes})
	return m, cmd
}

// submitInput sends the current input to the agent and clears the box. A
// single-line input starting with "/" is treated as a slash command.
func (m *Model) submitInput() tea.Cmd {
	line := strings.TrimSpace(m.textarea.Value())
	m.textarea.Reset()
	m.resizeInput()
	if line == "" {
		return nil
	}
	if !strings.Contains(line, "\n") && strings.HasPrefix(line, "/") {
		return m.handleCommand(line)
	}
	m.queue = append(m.queue, line)
	return m.nextTurn()
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
		m.appendLine(kindError, "approval error: "+err.Error())
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
	m.appendLine(kindUser, "> "+input)
	return runTurn(m.agent, m.ctx, input)
}

func (m *Model) appendEvent(ev agent.Event) {
	switch ev.Type {
	case agent.EventText:
		m.appendText(ev.Text)
	case agent.EventToolCall:
		m.appendLine(kindTool, fmt.Sprintf("[tool] %s %s", ev.Call.Name, ev.Call.Args))
	case agent.EventState:
		m.flushText()
	}
}
