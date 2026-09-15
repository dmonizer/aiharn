package tui

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/rivo/uniseg"

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

// shellBtnAction identifies a shell header button.
type shellBtnAction int

const (
	shellBtnClose shellBtnAction = iota
	shellBtnMaximize
)

// shellButton is a clickable header button in screen coordinates.
type shellButton struct {
	x, y, w int
	action  shellBtnAction
}

const commandPreviewLen = 80

var (
	styleCommand     = lipgloss.NewStyle().Foreground(lipgloss.Color("240")) // subdued gray chat command
	styleShellCmd    = lipgloss.NewStyle().Foreground(lipgloss.Color("6")).Bold(true)
	styleShellFocus  = lipgloss.NewStyle().Foreground(lipgloss.Color("15")).Background(lipgloss.Color("4"))
	styleShellBorder = lipgloss.NewStyle().Border(lipgloss.RoundedBorder())
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
		m.followShell()
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
			m.shellCmds[i].output = result
			m.shellCmds[i].done = true
			m.rebuildShellFlat()
			m.followShell()
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

// shellContentRows is the number of scrollable output rows: the pane height
// minus the border (2) and the header (1).
func (m *Model) shellContentRows() int {
	n := m.shellVisibleRows() - 3
	if n < 0 {
		return 0
	}
	return n
}

func (m *Model) shellMaxScroll() int {
	max := len(m.shellFlat) - m.shellContentRows()
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

// setShellScroll sets the scroll offset, re-arming autoscroll when pinned to the
// bottom and disabling it otherwise.
func (m *Model) setShellScroll(n int) {
	m.shellScroll = n
	m.clampShellScroll()
	m.shellFollow = m.shellScroll >= m.shellMaxScroll()
}

func (m *Model) scrollShell(delta int) {
	m.setShellScroll(m.shellScroll + delta)
}

// followShell keeps the view pinned to the newest output while the user has not
// scrolled away.
func (m *Model) followShell() {
	if m.shellFollow {
		m.shellScroll = m.shellMaxScroll()
	} else {
		m.clampShellScroll()
	}
}

func (m *Model) shellPageSize() int {
	n := m.shellContentRows()
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
	m.shellFollow = false
}

// clickTranscript maps a mouse click to a transcript command and opens it.
func (m *Model) clickTranscript(x, y int) {
	if m.shellMode == shellMaximized {
		return
	}
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

// clickShellButton dispatches a click on a shell header button.
func (m *Model) clickShellButton(x, y int) bool {
	for _, b := range m.shellButtons {
		if y == b.y && x >= b.x && x < b.x+b.w {
			switch b.action {
			case shellBtnClose:
				m.shellMode = shellClosed
			case shellBtnMaximize:
				if m.shellMode == shellMaximized {
					m.shellMode = shellOpen
				} else {
					m.shellMode = shellMaximized
				}
			}
			m.clampShellScroll()
			return true
		}
	}
	return false
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
			if m.clickShellButton(msg.X, msg.Y) {
				return nil
			}
			m.clickTranscript(msg.X, msg.Y)
		}
	}
	return nil
}

// shellPane renders the bordered shell session pane: a header with close and
// maximize buttons plus a scrollable, live view of every command and its output.
func (m *Model) shellPane(rows int, topY int) string {
	if rows <= 0 {
		return ""
	}
	m.clampShellScroll()

	innerH := rows - 2
	if innerH < 0 {
		innerH = 0
	}
	innerW := m.width - 2
	if innerW < 1 {
		innerW = 1
	}

	lines := []string{m.shellHeader(innerW, topY)}

	contentRows := innerH - 1
	if contentRows < 0 {
		contentRows = 0
	}
	end := m.shellScroll + contentRows
	if end > len(m.shellFlat) {
		end = len(m.shellFlat)
	}
	for i := m.shellScroll; i < end; i++ {
		raw := truncateToColumns(m.shellFlat[i], innerW)
		lines = append(lines, m.shellLineStyle(i, raw))
	}
	return styleShellBorder.Render(fill(strings.Join(lines, "\n"), innerH))
}

// shellHeader builds the pane header (title left, buttons right) and records the
// button positions for mouse handling. topY is the screen row of the border top.
func (m *Model) shellHeader(innerW, topY int) string {
	title := fmt.Sprintf("shell (%d)", len(m.shellCmds))
	closeLabel := "x"
	maxLabel := "[]"
	group := closeLabel + " " + maxLabel

	pad := innerW - len(title) - len(group)
	if pad < 1 {
		avail := innerW - len(group) - 1
		if avail < 1 {
			avail = 1
		}
		title = truncateToColumns(title, avail)
		pad = innerW - len(title) - len(group)
		if pad < 1 {
			pad = 1
		}
	}
	header := title + strings.Repeat(" ", pad) + group

	y := topY + 1 // first row inside the top border
	groupStart := innerW - len(group) + 1
	m.shellButtons = append(m.shellButtons,
		shellButton{x: groupStart, y: y, w: 1, action: shellBtnClose},
		shellButton{x: groupStart + 2, y: y, w: 2, action: shellBtnMaximize},
	)
	return header
}

func (m *Model) shellLineStyle(i int, text string) string {
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

// truncateToColumns cuts s to at most max grapheme columns, appending an
// ellipsis when truncated.
func truncateToColumns(s string, max int) string {
	if max <= 0 {
		return ""
	}
	var b strings.Builder
	w := 0
	gr := uniseg.NewGraphemes(s)
	for gr.Next() {
		g := gr.Str()
		gw := uniseg.StringWidth(g)
		if w+gw > max {
			return b.String() + "…"
		}
		b.WriteString(g)
		w += gw
	}
	return b.String()
}
