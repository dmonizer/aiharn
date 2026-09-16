package tui

import (
	"context"
	"errors"
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

	case tea.MouseMsg:
		return m, m.handleMouse(msg)

	case cursor.BlinkMsg:
		var cmd tea.Cmd
		m.textarea, cmd = m.textarea.Update(msg)
		return m, cmd

	case agentEventMsg:
		wasThinking := m.thinking
		m.appendEvent(msg.ev)
		wait := waitAgentEventContext(m.ctx, m.agent)
		if !wasThinking && m.thinking {
			return m, tea.Batch(wait, tickThinking())
		}
		return m, wait

	case approvalReqMsg:
		if _, cancelled := m.cancelledApprovals[msg.req.ID]; cancelled {
			delete(m.cancelledApprovals, msg.req.ID)
			return m, waitApprovalContext(m.ctx, m.gate)
		}
		if m.pending == nil {
			m.pending = &msg.req
		} else {
			m.approvals = append(m.approvals, msg.req)
		}
		return m, waitApprovalContext(m.ctx, m.gate)

	case rosterMsg:
		m.refreshSubagents()
		return m, waitRosterContext(m.ctx, m.manager)

	case bridgeStoppedMsg:
		return m, nil

	case thinkingTickMsg:
		if m.thinking {
			return m, tickThinking()
		}
		return m, nil

	case turnDoneMsg:
		m.finishThinking(time.Now())
		m.flushText()
		m.running = false
		if m.turnCancel != nil {
			m.turnCancel()
			m.turnCancel = nil
		}
		expectedCancel := m.stopping && errors.Is(msg.err, context.Canceled)
		m.stopping = false
		if msg.err != nil && !expectedCancel {
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

	// Terminals encode Ctrl+Esc identically to Esc. While work is active, that
	// key therefore means "stop everything"; while idle, Esc retains its
	// existing double-press-to-clear behavior.
	if msg.String() == "esc" && m.stopAllRequests() {
		return m, nil
	}

	if msg.String() == "f10" {
		m.showReasoning = !m.showReasoning
		return m, nil
	}

	if m.pending != nil {
		return m, m.handleApprovalKey(msg)
	}

	if msg.String() == "ctrl+s" {
		m.cycleShell()
		return m, nil
	}

	switch msg.String() {
	case "ctrl+up":
		m.resizeInputBy(1)
		return m, nil
	case "ctrl+down":
		m.resizeInputBy(-1)
		return m, nil
	}

	if m.shellMode == shellMaximized {
		switch msg.String() {
		case "up":
			m.scrollShell(-1)
		case "down":
			m.scrollShell(1)
		case "pgup":
			m.scrollShell(-m.shellPageSize())
		case "pgdown":
			m.scrollShell(m.shellPageSize())
		case "home":
			m.setShellScroll(0)
		case "end":
			m.setShellScroll(m.shellMaxScroll())
		}
		return m, nil
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

	case "up":
		m.historyUp()
		m.resizeInput()
		return m, nil

	case "down":
		m.historyDown()
		m.resizeInput()
		return m, nil

	case "pgup", "pgdown", "home", "end":
		if m.shellMode != shellClosed {
			switch msg.String() {
			case "pgup":
				m.scrollShell(-m.shellPageSize())
			case "pgdown":
				m.scrollShell(m.shellPageSize())
			case "home":
				m.setShellScroll(0)
			case "end":
				m.setShellScroll(m.shellMaxScroll())
			}
			return m, nil
		}
		if msg.String() == "pgup" {
			m.pageInput(-1)
			return m, nil
		}
		if msg.String() == "pgdown" {
			m.pageInput(1)
			return m, nil
		}
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

// stopAllRequests cancels the top-level turn, all active agent turns, queued
// top-level prompts, queued subagent tasks, and approval UI state. It returns
// false when there was no work, allowing Esc to keep its input-clearing role.
func (m *Model) stopAllRequests() bool {
	hadWork := m.running || len(m.queue) > 0 || m.pending != nil || len(m.approvals) > 0
	if m.turnCancel != nil {
		m.turnCancel()
	}
	if m.manager != nil {
		if n, err := m.manager.CancelAll(); err == nil && n > 0 {
			hadWork = true
		}
	}
	if m.gate != nil {
		pending := m.gate.PendingRequests()
		if len(pending) > 0 && m.cancelledApprovals == nil {
			m.cancelledApprovals = make(map[string]struct{}, len(pending))
		}
		for _, req := range pending {
			hadWork = true
			m.cancelledApprovals[req.ID] = struct{}{}
			_ = m.gate.Decide(req.ID, approval.DecisionDenied)
		}
	}
	if !hadWork {
		return false
	}
	m.queue = nil
	m.pending = nil
	m.approvals = nil
	m.lastEsc = time.Time{}
	m.stopping = m.running
	m.appendLine(kindPlain, "stopping all active requests")
	return true
}

// submitInput sends the current input to the agent and clears the box. A
// single-line input starting with "/" is treated as a slash command.
func (m *Model) submitInput() tea.Cmd {
	line := strings.TrimSpace(m.textarea.Value())
	m.textarea.Reset()
	m.resizeInput()
	m.histIdx = -1
	if line == "" {
		m.draft = ""
		return nil
	}
	if !strings.Contains(line, "\n") && strings.HasPrefix(line, "/") {
		m.draft = ""
		return m.handleCommand(line)
	}
	m.pushHistory(line)
	m.draft = ""
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
	turnCtx, cancel := context.WithCancel(m.ctx)
	m.turnCancel = cancel
	return runTurn(m.agent, turnCtx, input)
}

func (m *Model) beginThinking(now time.Time) {
	if m.thinking {
		return
	}
	m.thinking = true
	m.thinkingStarted = now
}

func (m *Model) finishThinking(now time.Time) {
	if !m.thinking {
		return
	}
	m.flushText()
	m.lines = append(m.lines, line{
		text: fmt.Sprintf("thinking ... (%s)", formatThinkingElapsed(now.Sub(m.thinkingStarted))),
		kind: kindReasoningStatus, cmd: -1,
	})
	m.trimLines()
	m.thinking = false
	m.thinkingStarted = time.Time{}
}

func formatThinkingElapsed(elapsed time.Duration) string {
	if elapsed < 0 {
		elapsed = 0
	}
	totalSeconds := int(elapsed / time.Second)
	return fmt.Sprintf("%02d:%02d", totalSeconds/60, totalSeconds%60)
}

func (m *Model) appendEvent(ev agent.Event) {
	switch ev.Type {
	case agent.EventReasoningStart:
		m.beginThinking(time.Now())
	case agent.EventReasoningDelta:
		if !m.thinking {
			m.beginThinking(time.Now())
		}
		m.appendReasoning(ev.Text)
	case agent.EventText:
		m.finishThinking(time.Now())
		m.appendText(ev.Text)
	case agent.EventToolCall:
		m.finishThinking(time.Now())
		m.appendToolCall(ev.Call)
	case agent.EventToolResult:
		m.appendToolResult(ev.Call, ev.Text)
	case agent.EventState:
		if ev.State != agent.StateRunning {
			m.finishThinking(time.Now())
		}
		m.flushText()
	}
}
