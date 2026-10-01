package tui

import (
	"context"
	"errors"
	"fmt"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// requestClear stops the current session's work and returns a command that
// builds a replacement session without blocking Update.
func (m *Model) requestClear() tea.Cmd {
	if m.clear == nil {
		m.appendLine(kindError, "clear is not available in this session")
		return nil
	}
	if m.clearing {
		return nil
	}
	m.clearing = true
	m.stopAllRequests()
	m.appendLine(kindPlain, "clearing session...")
	return runClear(m.clear, m.sessionGen)
}

// runClear runs the injected clear callback in a goroutine and reports its
// result as a clearDoneMsg.
func runClear(clear ClearFunc, gen uint64) tea.Cmd {
	return func() tea.Msg {
		if clear == nil {
			return clearDoneMsg{gen: gen, err: errors.New("clear is not available")}
		}
		result, err := clear(context.Background())
		return clearDoneMsg{gen: gen, result: result, err: err}
	}
}

// rebind switches the TUI to a replacement session. It cancels the previous
// session's bridges, advances the generation so any already-queued messages
// from the old runtime are dropped, and resets the presentation state.
func (m *Model) rebind(result ClearResult) {
	m.clearing = false
	m.cancel()
	m.sessionGen++

	ctx, cancel := context.WithCancel(context.Background())
	m.ctx = ctx
	m.cancel = cancel
	m.manager = result.Manager
	m.agent = result.Agent
	m.gate = result.Gate
	if result.Model != "" {
		m.status.Model = result.Model
	}
	if result.AgentType != "" {
		m.status.AgentType = result.AgentType
	}
	if result.Channel != "" {
		m.status.Channel = result.Channel
	}
	if result.Approval != "" {
		m.status.Approval = result.Approval
	}
	if result.Agent != nil {
		m.focusedID = result.Agent.ID()
	} else {
		m.focusedID = ""
	}

	m.resetForSession()
	m.appendLine(kindPlain, fmt.Sprintf("aiharn: agent %s · model %s · channel %s · approval %s",
		m.status.AgentType, m.status.Model, m.status.Channel, m.status.Approval))
	m.refreshSubagents()
}

// resetForSession clears the conversation-specific presentation state. It keeps
// user preferences such as reasoning visibility and input-history/input-size.
func (m *Model) resetForSession() {
	m.flushText()
	m.lines = nil
	m.curText = nil
	m.curKind = kindPlain
	m.queue = nil
	m.running = false
	m.stopping = false
	m.turnCancel = nil

	m.pending = nil
	m.approvals = nil
	m.cancelledApprovals = nil
	m.approvalPopover = false
	m.approvalScroll = 0
	m.approvalLinkHit = approvalHit{}
	m.approvalCloseHit = approvalHit{}
	m.toolLimit = nil
	m.toolLimitQueue = nil

	m.views = make(map[string]agentView)
	m.bridged = make(map[string]bool)
	m.subagents = nil

	m.shellCmds = nil
	m.shellFlat = nil
	m.shellStart = nil
	m.shellRowCmd = nil
	m.shellMode = shellClosed
	m.shellScroll = 0
	m.shellFocus = -1
	m.shellFollow = true
	m.shellButtons = nil

	m.clickRows = nil
	m.clickWidth = 0
	m.expandedToolGroups = make(map[int]bool)
	m.nextToolGroupID = 0
	m.chatGroupHits = nil
	m.groupHitRows = nil
	m.chatScroll = 0
	m.chatFollow = true

	m.rosterHits = nil
	m.hoverX = -1
	m.hoverY = -1

	m.thinking = false
	m.thinkingStarted = time.Time{}

	m.histIdx = -1
	m.draft = ""
	m.textarea.Reset()
	m.resizeInput()
}
