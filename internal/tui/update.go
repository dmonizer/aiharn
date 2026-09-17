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
	"aiharn/internal/llm"
)

// Update implements tea.Model. It dispatches on message type and returns the
// commands to keep the bridges and turn loop alive.
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.resizeInput()
		m.followShell()
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
		id := msg.ev.AgentID
		if id == "" && m.agent != nil {
			id = m.agent.ID()
		}
		wasThinking := m.thinking && m.focusedID == id
		m.appendAgentEvent(msg.ev)
		if msg.ev.Type == agent.EventState && (msg.ev.State == agent.StateClosed || msg.ev.State == agent.StateErrored) {
			return m, nil // terminal subagent streams need no permanently blocked bridge
		}
		a := m.agent
		if m.manager != nil && id != "" {
			a = m.manager.Agent(id)
		}
		if a == nil {
			return m, nil
		}
		wait := waitAgentEventContext(m.ctx, a)
		if m.focusedID == id && !wasThinking && m.thinking {
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
		return m, tea.Batch(waitRosterContext(m.ctx, m.manager), m.startSubagentBridges())

	case subagentClosedMsg:
		if msg.err != nil {
			m.appendLine(kindError, "close subagent: "+msg.err.Error())
		}
		m.refreshSubagents()
		return m, nil

	case bridgeStoppedMsg:
		return m, nil

	case thinkingTickMsg:
		if m.thinking {
			return m, tickThinking()
		}
		return m, nil

	case turnDoneMsg:
		rootID := ""
		if m.agent != nil {
			rootID = m.agent.ID()
		}
		return m, m.withAgentView(rootID, func() tea.Cmd {
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
			return m.nextTurn()
		})
	}
	return m, nil
}

func (m *Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if msg.String() == "ctrl+c" {
		m.cancel()
		return m, tea.Quit
	}
	if m.approvalPopover && m.pending == nil {
		m.approvalPopover = false
	}
	if m.approvalPopover {
		switch msg.String() {
		case "esc", "enter":
			m.approvalPopover = false
		case "up":
			m.scrollApproval(-1)
		case "down":
			m.scrollApproval(1)
		case "pgup":
			m.scrollApproval(-m.approvalPageSize())
		case "pgdown":
			m.scrollApproval(m.approvalPageSize())
		case "home":
			m.approvalScroll = 0
		case "end":
			m.scrollApproval(m.approvalMaxScroll())
		}
		return m, nil
	}

	// Terminals encode Ctrl+Esc identically to Esc. While work is active, that
	// key therefore means "stop everything"; while idle, Esc retains its
	// existing double-press-to-clear behavior.
	if msg.String() == "esc" && m.stopAllRequests() {
		return m, nil
	}

	if msg.String() == m.shortcuts.ToggleThinking {
		m.showReasoning = !m.showReasoning
		return m, nil
	}
	if msg.String() == m.shortcuts.ToggleActions && m.gate != nil {
		if m.gate.Mode() == approval.ModeAsk {
			m.gate.SetMode(approval.ModeAllowAll)
		} else {
			m.gate.SetMode(approval.ModeAsk)
		}
		return m, nil
	}

	if m.pending != nil {
		return m, m.handleApprovalKey(msg)
	}

	if msg.String() == m.shortcuts.CycleShell {
		m.cycleShell()
		return m, nil
	}

	switch msg.String() {
	case m.shortcuts.GrowInput:
		m.resizeInputBy(1)
		return m, nil
	case m.shortcuts.ShrinkInput:
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
	m.approvalPopover = false
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
	if m.agent != nil && m.focusedID != "" && m.focusedID != m.agent.ID() {
		// A person typed this line into the focused subagent, so it is human-authored.
		if err := m.manager.SendSubagentMessage(m.ctx, m.agent.ID(), m.focusedID, line, llm.OriginHuman); err != nil {
			m.appendLine(kindError, "send to subagent: "+err.Error())
		} else {
			m.appendLine(kindPlain, "[queued for "+m.focusedID+"]")
		}
		return nil
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
	m.approvalPopover = false
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
	case agent.EventUser:
		if m.agent != nil && ev.AgentID != m.agent.ID() {
			m.appendLine(kindUser, "> "+ev.Text)
		}
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
	case agent.EventAgentMessage:
		// Rendered for every agent, not just the focused one: agent-to-agent
		// traffic is cross-agent and must never be hidden by focus.
		m.appendAgentMessage(ev)
	}
}

// appendAgentMessage renders one agent-to-agent message as its own line. The
// header names the direction (↑ up / ↓ down), the sender, the recipient, and
// the kind, so the line is immediately separable from human input and assistant
// output. A pending message keeps its real direction arrow and gains a
// "(pending)" marker, and its body is shown too, so a queued message reads the
// same as a delivered one plus the marker. The body follows the header, and
// appendLine handles multi-line text and terminal sanitization.
func (m *Model) appendAgentMessage(ev agent.Event) {
	// Flush streamed assistant text first so a pending delta cannot be woven
	// into (or mistaken for) the message body.
	m.flushText()
	header := agentMessageHeader(ev)
	// Show the body for both delivered and pending messages. The lone exception
	// preserves the nil-Delivery fallback exactly as it was: a malformed pending
	// event renders just "(agent message) (pending)".
	showBody := ev.Text != "" && (ev.Delivery != nil || !ev.Pending)
	if showBody {
		header += "  " + ev.Text
	}
	m.appendLine(kindAgentMessage, header)
}

// agentMessageHeader builds the provenance header for an agent message. The
// direction arrow is kept while pending; only the trailing "(pending)" marker
// signals that the message is still queued. It is defensive about a nil
// (malformed or stale) Delivery instead of panicking.
func agentMessageHeader(ev agent.Event) string {
	d := ev.Delivery
	if d == nil {
		header := "(agent message)"
		if ev.Pending {
			header += " (pending)"
		}
		return header
	}
	arrow := "→"
	switch d.Direction {
	case llm.DirectionUp:
		arrow = "↑"
	case llm.DirectionDown:
		arrow = "↓"
	}
	header := fmt.Sprintf("%s %s → %s · %s", arrow, d.From, d.To, d.Kind)
	if ev.Pending {
		header += " (pending)"
	}
	return header
}
