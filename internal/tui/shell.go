package tui

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/charmbracelet/lipgloss"
	tea "github.com/charmbracelet/bubbletea"

	"aiharn/internal/llm"
	"aiharn/internal/tools"
)

// shellViewMode is the visibility state of the shell session pane.
type shellViewMode int

const (
	shellClosed shellViewMode = iota
	shellOpen
	shellMaximized
)

// shellCmd is one executed shell command and its (completed or in-progress)
// output.
type shellCmd struct {
	id      string // tool-call id, matching the result to the call
	command string // normalized single-line command
	output  string // full result text ("" while running)
	done    bool
}

const commandPreviewLen = 80

var (
	styleCommand    = lipgloss.NewStyle().Foreground(lipgloss.Color("240")) // subdued gray chat command
	styleShellCmd   = lipgloss.NewStyle().Foreground(lipgloss.Color("6")).Bold(true)
	styleShellFocus = lipgloss.NewStyle().Foreground(lipgloss.Color("15")).Background(lipgloss.Color("4"))
	styleShellHead  = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
)

// appendToolCall records a tool call. execute_command becomes a clickable shell
// command; every other tool is rendered as a plain tool line.
func (m *Model) appendToolCall(call llm.Item) {
	if call.Name == tools.NameExecuteCommand {
		cmd := shellCmd{id: call.CallID, command: normalizeCommand(extractCommand(call.Args))}
		m.shellCmds = append(m.shellCmds, cmd)
		idx := len(m.shellCmds) - 1
		m.appendCommandLine(idx, truncateCommand(cmd.command, commandPreviewLen))
		m.rebuildShellFlat()
		return
	}
	m.appendLine(kindTool, fmt.Sprintf("[tool] %s %s", call.Name, call.Args))
}

// appendToolResult attaches a tool result to its command, updating the shell view.
func (m *Model) appendToolResult(call llm.Item, result string) {
	if call.Name != tools.NameExecuteCommand {
		return
	}
	for i := range m.shellCmds {
		if m.shellCmds[i].id == call.CallID && !m.shellCmds[i].done {
			follow := m.shellScroll >= m.shellMaxScroll()
			m.shellCmds[i].output = result
			m.shellCmds[i].done = true
			m.rebuildShellFlat()
			if follow {
				m.shellScroll = m.shellMaxScroll()
			} else {
				m.clampShellScroll()
			}
			return
		}
	}
}

// appendCommandLine flushes any streamed text and appends a clickable command
// line referencing shellCmds[idx].
func (m *Model) appendCommandLine(idx int, text string) {
	m.flushText()
	m.lines = append(m.lines, line{text: text, kind: kindCommand, cmd: idx})
	m.trimLines()
}

// rebuildShellFlat flattens the command list into display rows for the shell pane.
func (m *Model) rebuildShellFlat() {
	m.shellFlat = m.shellFlat[:0]
	m.shellStart = m.shellStart[:0]
	m.shellRowCmd = m.shellRowCmd[:0]
	for i := range m.shellCmds {
		c := &m.shellCmds[i]
		m.shellStart = append(m.shellStart, len(m.shellFlat))
		status := ""
		if !c.done {
			status = " …"
		}
		m.shellFlat = append(m.shellFlat, "$ "+c.command+status)
		m.shellRowCmd = append(m.shellRowCmd, i)
		for _, ol := range strings.Split(c.output, "\n") {
			m.shellFlat = append(m.shellFlat, ol)
			m.shellRowCmd = append(m.shellRowCmd, -1)
		}
	}
}

// cycleShell advances the shell pane closed → open → maximized → closed.
func (m *Model) cycleShell() {
	switch m.shellMode {
	case shellClosed:
		m.shellMode = shellOpen
	case shellOpen:
		m.shellMode = shellMaximized
	case shellMaximized:
		m.shellMode = shellClosed
	}
	m.clampShellScroll()
}

func (m *Model) shellOpenRows() int {
	total := m.rows()
	n := total / 2
	if n < 1 && total > 0 {
		n = 1
	}
	return n
}

func (m *Model) shellVisibleRows() int {
	switch m.shellMode {
	case shellMaximized:
		if m.height-1 < 0 {
			return 0
		}
		return m.height - 1
	case shellOpen:
		return m.shellOpenRows()
	default:
		return 0
	}
}

func (m *Model) shellMaxScroll() int {
	content := m.shellVisibleRows() - 1 // one row is the pane header
	if content < 0 {
		content = 0
	}
	max := len(m.shellFlat) - content
	if max < 0 {
		max = 0
	}
	return max
}

func (m *Model) clampShellScroll() {
	max := m.shellMaxScroll()
	if m.shellScroll > max {
		m.shellScroll = max
	}
	if m.shellScroll < 0 {
		m.shellScroll = 0
	}
}

func (m *Model) scrollShell(delta int) {
	m.shellScroll += delta
	m.clampShellScroll()
}

func (m *Model) shellPageSize() int {
	n := m.shellVisibleRows() - 1
	if n < 1 {
		n = 1
	}
	return n
}

// openShellFocus opens the shell pane (if closed) and focuses the given command.
func (m *Model) openShellFocus(idx int) {
	if idx < 0 || idx >= len(m.shellCmds) {
		return
	}
	if m.shellMode == shellClosed {
		m.shellMode = shellOpen
	}
	m.shellFocus = idx
	if idx < len(m.shellStart) {
		m.shellScroll = m.shellStart[idx]
	}
	m.clampShellScroll()
}

// clickTranscript maps a mouse click to a transcript command and opens it.
func (m *Model) clickTranscript(x, y int) {
	r := y - 1 // body starts below the status line
	if r < 0 || r >= len(m.clickRows) {
		return
	}
	if m.clickWidth > 0 && x >= m.clickWidth {
		return
	}
	if idx := m.clickRows[r]; idx >= 0 {
		m.openShellFocus(idx)
	}
}

func (m *Model) handleMouse(msg tea.MouseMsg) tea.Cmd {
	switch msg.Button {
	case tea.MouseButtonWheelUp:
		if m.shellMode != shellClosed {
			m.scrollShell(-3)
		}
	case tea.MouseButtonWheelDown:
		if m.shellMode != shellClosed {
			m.scrollShell(3)
		}
	case tea.MouseButtonLeft:
		if msg.Action == tea.MouseActionPress {
			m.clickTranscript(msg.X, msg.Y)
		}
	}
	return nil
}

// shellPane renders the shell session pane: a header plus a scrollable, live view
// of every command and its output.
func (m *Model) shellPane(rows int) string {
	if rows <= 0 {
		return ""
	}
	m.clampShellScroll()
	lines := []string{styleShellHead.Render(fmt.Sprintf("shell (%d commands)", len(m.shellCmds)))}
	content := rows - 1
	end := m.shellScroll + content
	if end > len(m.shellFlat) {
		end = len(m.shellFlat)
	}
	for i := m.shellScroll; i < end; i++ {
		lines = append(lines, m.shellLineStyle(i))
	}
	return fill(strings.Join(lines, "\n"), rows)
}

func (m *Model) shellLineStyle(i int) string {
	text := m.shellFlat[i]
	if cmd := m.shellRowCmd[i]; cmd >= 0 {
		if cmd == m.shellFocus {
			return styleShellFocus.Render(text)
		}
		return styleShellCmd.Render(text)
	}
	return text
}

// extractCommand pulls the shell command out of execute_command arguments JSON.
func extractCommand(args string) string {
	var p struct {
		Command string `json:"command"`
		Cwd     string `json:"cwd"`
	}
	if err := json.Unmarshal([]byte(args), &p); err == nil && p.Command != "" {
		return p.Command
	}
	return args
}

// normalizeCommand collapses whitespace (including newlines) to single spaces.
func normalizeCommand(s string) string {
	return strings.Join(strings.Fields(sanitizeTerminalText(s)), " ")
}

// truncateCommand shortens s to at most max bytes on a UTF-8 boundary.
func truncateCommand(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}
