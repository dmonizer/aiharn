// Package tui implements the terminal UI. It is a thin Bubbletea layer over the
// agent runtime: the manager, focused agent, and approval gate are injected,
// their events are bridged into tea messages, and the model renders a focused
// transcript, a subagent list, and an input line. It holds no business logic.
package tui

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/charmbracelet/bubbles/textarea"
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

// lineKind discriminates a transcript line for color styling.
type lineKind int

const (
	kindPlain lineKind = iota
	kindUser
	kindAssistant
	kindTool
	kindCommand
	kindError
)

// line is one flushed transcript line with its presentation kind. cmd indexes
// into the shell command list for clickable command lines, or -1.
type line struct {
	text string
	kind lineKind
	cmd  int
}

// Model is the Bubbletea root model.
type Model struct {
	manager *agent.Manager
	agent   *agent.Agent // the focused (top-level) agent
	gate    *approval.Gate
	status  Status

	lines   []line   // flushed transcript lines, oldest first
	curText []byte   // streamed text not yet flushed to a line
	textarea textarea.Model
	queue   []string // inputs waiting for the agent to become idle
	running bool

	lastEsc time.Time // when a first ESC press armed the clear, zero if none

	subagents []tools.SubagentStatus // current roster snapshot

	pending   *approval.Request  // active approval modal, nil when none
	approvals []approval.Request // additional requests waiting behind the modal

	// shell session view
	shellCmds   []shellCmd
	shellFlat   []string // flattened display rows (header + output)
	shellStart  []int    // per command, index of its header row in shellFlat
	shellRowCmd []int    // per shellFlat row, owning command index or -1
	shellMode   shellViewMode
	shellScroll int
	shellFocus  int // index into shellCmds, -1 = none

	// click hit-testing (computed in View, consumed by mouse handling)
	clickRows  []int // per transcript row: shellCmds index or -1
	clickWidth int   // transcript pane width; 0 = no column constraint

	// input history
	history []string
	histIdx int    // -1 = not navigating (draft/empty area)
	draft   string // saved half-finished prompt during navigation

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
	m.textarea = newTextarea()
	m.textarea.Focus()
	m.resizeInput()
	m.shellFocus = -1
	m.histIdx = -1
	m.appendLine(kindPlain, fmt.Sprintf("aiharn: agent %s · model %s · channel %s · approval %s",
		status.AgentType, status.Model, status.Channel, status.Approval))
	m.refreshSubagents()
	return m
}

// Init starts the agent-event, approval, and roster bridges.
func (m *Model) Init() tea.Cmd {
	return tea.Batch(waitAgentEventContext(m.ctx, m.agent), waitApprovalContext(m.ctx, m.gate), waitRosterContext(m.ctx, m.manager), m.textarea.Focus())
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

// appendLine appends a completed transcript line with the given kind.
func (m *Model) appendLine(kind lineKind, s string) {
	m.flushText()
	for _, part := range strings.Split(sanitizeTerminalText(s), "\n") {
		m.lines = append(m.lines, line{text: part, kind: kind, cmd: -1})
	}
	m.trimLines()
}

// appendText accumulates a streamed text delta, flushing complete lines.
func (m *Model) appendText(s string) {
	m.curText = append(m.curText, sanitizeTerminalText(s)...)
	if len(m.curText) > maxPartialTextBytes {
		const marker = "[earlier streamed text truncated]"
		keep := maxPartialTextBytes - len(marker)
		cut := len(m.curText) - keep
		for cut < len(m.curText) && !utf8.RuneStart(m.curText[cut]) {
			cut++
		}
		m.curText = append([]byte(marker), m.curText[cut:]...)
	}
	for {
		i := bytes.IndexByte(m.curText, '\n')
		if i < 0 {
			return
		}
		m.lines = append(m.lines, line{text: string(m.curText[:i]), kind: kindAssistant, cmd: -1})
		m.trimLines()
		m.curText = m.curText[i+1:]
	}
}

const (
	maxBufferedTranscriptLines = 10000
	maxPartialTextBytes        = 1 << 20
)

func (m *Model) trimLines() {
	if len(m.lines) > maxBufferedTranscriptLines {
		m.lines = append([]line(nil), m.lines[len(m.lines)-maxBufferedTranscriptLines:]...)
	}
}

// flushText moves any partial streamed text into the transcript.
func (m *Model) flushText() {
	if len(m.curText) != 0 {
		m.lines = append(m.lines, line{text: string(m.curText), kind: kindAssistant, cmd: -1})
		m.curText = nil
		m.trimLines()
	}
}
