package tui

import (
	"fmt"
	"time"

	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"

	"aiharn/internal/agent"
)

// agentView contains presentation state only. Every agent keeps its own chat,
// shell output, prompt draft and input history while the root turn/approval
// machinery continues to run independently of which view is selected.
type agentView struct {
	lines           []line
	curText         []byte
	curKind         lineKind
	textarea        textarea.Model
	inputMax        int
	history         []string
	histIdx         int
	draft           string
	shellCmds       []shellCmd
	shellFlat       []string
	shellStart      []int
	shellRowCmd     []int
	shellMode       shellViewMode
	shellScroll     int
	shellFocus      int
	shellFollow     bool
	chatScroll      int
	chatFollow      bool
	thinking        bool
	thinkingStarted time.Time
}

func (m *Model) currentView() agentView {
	return agentView{
		lines: m.lines, curText: m.curText, curKind: m.curKind,
		textarea: m.textarea, inputMax: m.inputMax,
		history: m.history, histIdx: m.histIdx, draft: m.draft,
		shellCmds: m.shellCmds, shellFlat: m.shellFlat, shellStart: m.shellStart,
		shellRowCmd: m.shellRowCmd, shellMode: m.shellMode, shellScroll: m.shellScroll,
		shellFocus: m.shellFocus, shellFollow: m.shellFollow,
		chatScroll: m.chatScroll, chatFollow: m.chatFollow,
		thinking: m.thinking, thinkingStarted: m.thinkingStarted,
	}
}

func (m *Model) loadView(v agentView) {
	m.lines, m.curText, m.curKind = v.lines, v.curText, v.curKind
	m.textarea, m.inputMax = v.textarea, v.inputMax
	m.history, m.histIdx, m.draft = v.history, v.histIdx, v.draft
	m.shellCmds, m.shellFlat, m.shellStart = v.shellCmds, v.shellFlat, v.shellStart
	m.shellRowCmd, m.shellMode, m.shellScroll = v.shellRowCmd, v.shellMode, v.shellScroll
	m.shellFocus, m.shellFollow = v.shellFocus, v.shellFollow
	m.chatScroll, m.chatFollow = v.chatScroll, v.chatFollow
	m.thinking, m.thinkingStarted = v.thinking, v.thinkingStarted
}

func (m *Model) focusAgent(id string) {
	if id == "" || id == m.focusedID || m.manager == nil || m.manager.Agent(id) == nil {
		return
	}
	m.views[m.focusedID] = m.currentView()
	if v, ok := m.views[id]; ok {
		m.loadView(v)
	} else {
		m.loadView(agentView{textarea: newTextarea(m.shortcuts), inputMax: defaultInputHeight, histIdx: -1, shellFocus: -1, shellFollow: true, chatFollow: true})
		m.textarea.Focus()
		m.appendLine(kindPlain, fmt.Sprintf("agent %s (%s)", id, m.manager.Agent(id).Type()))
	}
	m.focusedID = id
	m.resizeInput()
}

func (m *Model) appendAgentEvent(ev agent.Event) {
	if ev.AgentID == "" || ev.AgentID == m.focusedID || m.focusedID == "" {
		m.appendEvent(ev)
		return
	}
	if m.manager == nil || m.manager.Agent(ev.AgentID) == nil {
		return
	}
	focused := m.currentView()
	if v, ok := m.views[ev.AgentID]; ok {
		m.loadView(v)
	} else {
		m.loadView(agentView{textarea: newTextarea(m.shortcuts), inputMax: defaultInputHeight, histIdx: -1, shellFocus: -1, shellFollow: true, chatFollow: true})
		m.appendLine(kindPlain, fmt.Sprintf("agent %s (%s)", ev.AgentID, m.manager.Agent(ev.AgentID).Type()))
	}
	m.appendEvent(ev)
	m.views[ev.AgentID] = m.currentView()
	m.loadView(focused)
}

// withAgentView mutates an agent's stored presentation state without changing
// the user's current focus. Root turn completions can arrive while a subagent
// is selected, and must not flush or append to that subagent's transcript.
func (m *Model) withAgentView(id string, fn func() tea.Cmd) tea.Cmd {
	if id == "" || id == m.focusedID {
		return fn()
	}
	focused := m.currentView()
	m.loadView(m.views[id])
	cmd := fn()
	m.views[id] = m.currentView()
	m.loadView(focused)
	return cmd
}

// Each event channel has exactly one bridge, including while its agent is not
// focused. Draining background streams preserves independent live contexts.
func (m *Model) startSubagentBridges() tea.Cmd {
	var cmds []tea.Cmd
	for _, sub := range m.subagents {
		if m.bridged[sub.ID] {
			continue
		}
		a := m.manager.Agent(sub.ID)
		if a == nil {
			continue
		}
		m.bridged[sub.ID] = true
		cmds = append(cmds, waitAgentEventContext(m.ctx, a))
	}
	return tea.Batch(cmds...)
}
