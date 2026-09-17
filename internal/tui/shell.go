package tui

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

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

const commandPreviewLen = 70

var (
	styleCommand     = lipgloss.NewStyle().Foreground(lipgloss.Color("240")) // subdued gray chat command
	styleShellCmd    = lipgloss.NewStyle().Foreground(lipgloss.Color("6")).Bold(true)
	styleShellFocus  = lipgloss.NewStyle().Foreground(lipgloss.Color("15")).Background(lipgloss.Color("4"))
	styleShellScroll = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
	styleShellBorder = lipgloss.NewStyle().Border(lipgloss.RoundedBorder())
)

// appendToolCall records a tool call. execute_command becomes a clickable shell
// command; every other tool is rendered as a plain tool line.
func (m *Model) appendToolCall(call llm.Item) {
	if call.Name == tools.NameExecuteCommand {
		cmd := shellCmd{id: call.CallID, command: normalizeCommand(extractCommand(call.Args))}
		m.shellCmds = append(m.shellCmds, cmd)
		idx := len(m.shellCmds) - 1
		preview, truncated := previewCommand(cmd.command, commandPreviewLen)
		if truncated {
			preview += "..."
		}
		m.appendCommandLine(idx, preview)
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
			// Command output is untrusted: strip control sequences and expand
			// tabs so each row's measured width matches what it renders.
			m.shellFlat = append(m.shellFlat, sanitizeTerminalText(ol))
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
	m.followShell()
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
	// A click on the newest command means "show its live output", so keep that
	// view pinned as the result grows. Older commands remain a stable focus and
	// deliberately disable following.
	if idx == len(m.shellCmds)-1 {
		m.shellFollow = true
		m.followShell()
		return
	}
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
	r := y // body starts at the top; status is below the input
	if r < 0 || r >= len(m.clickRows) {
		return
	}
	if m.clickWidth > 0 && (x <= 0 || x >= m.clickWidth-1) {
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
			m.followShell()
			return true
		}
	}
	return false
}

// clickShellScrollbar jumps to the portion of output indicated by a click or
// left-button drag on the visible scrollbar track.
func (m *Model) clickShellScrollbar(x, y int) bool {
	contentRows := m.shellContentRows()
	if m.shellMode == shellClosed || contentRows <= 0 || len(m.shellFlat) <= contentRows || m.width <= 3 || x != m.width-2 {
		return false
	}
	topY := 0
	if m.shellMode == shellOpen {
		topY += m.rows() - m.shellOpenRows()
	}
	first := topY + 2
	if y < first || y >= first+contentRows {
		return false
	}
	if contentRows == 1 {
		m.setShellScroll(m.shellMaxScroll())
		return true
	}
	m.setShellScroll((y - first) * m.shellMaxScroll() / (contentRows - 1))
	return true
}

func (m *Model) handleMouse(msg tea.MouseMsg) tea.Cmd {
	if m.pending == nil {
		m.approvalPopover = false
	}
	if m.approvalPopover {
		m.hoverX, m.hoverY = msg.X, msg.Y
		switch msg.Button {
		case tea.MouseButtonWheelUp:
			m.scrollApproval(-3)
		case tea.MouseButtonWheelDown:
			m.scrollApproval(3)
		case tea.MouseButtonLeft:
			if msg.Action == tea.MouseActionPress && m.approvalCloseHit.contains(msg.X, msg.Y) {
				m.approvalPopover = false
			}
		}
		return nil
	}
	if msg.Action == tea.MouseActionMotion {
		m.hoverX, m.hoverY = msg.X, msg.Y
		if msg.Button == tea.MouseButtonLeft {
			m.clickShellScrollbar(msg.X, msg.Y)
		}
		return nil
	}
	switch msg.Button {
	case tea.MouseButtonWheelUp:
		if m.pending == nil && m.shellMode != shellMaximized && msg.Y >= m.rows() && msg.Y < m.height-1 {
			m.scrollInput(-3)
			return nil
		}
		if m.shellMode != shellClosed {
			m.scrollShell(-3)
		}
	case tea.MouseButtonWheelDown:
		if m.pending == nil && m.shellMode != shellMaximized && msg.Y >= m.rows() && msg.Y < m.height-1 {
			m.scrollInput(3)
			return nil
		}
		if m.shellMode != shellClosed {
			m.scrollShell(3)
		}
	case tea.MouseButtonLeft:
		if msg.Action == tea.MouseActionPress {
			if m.pending != nil && m.approvalLinkHit.contains(msg.X, msg.Y) {
				m.approvalPopover = true
				m.approvalScroll = 0
				return nil
			}
			if m.clickShellScrollbar(msg.X, msg.Y) {
				return nil
			}
			if handled, cmd := m.clickRoster(msg.X, msg.Y); handled {
				return cmd
			}
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
	m.followShell()

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
	showScrollbar := len(m.shellFlat) > contentRows && contentRows > 0
	contentW := innerW
	if showScrollbar && contentW > 1 {
		contentW--
	}
	end := m.shellScroll + contentRows
	if end > len(m.shellFlat) {
		end = len(m.shellFlat)
	}
	thumbStart, thumbSize := m.shellScrollbar(contentRows)
	for i := m.shellScroll; i < end; i++ {
		raw := truncateToColumns(m.shellFlat[i], contentW)
		line := m.shellLineStyle(i, raw)
		if showScrollbar && innerW > 1 {
			line = lipgloss.NewStyle().Width(contentW).Render(line)
			trackRow := i - m.shellScroll
			glyph := "│"
			if trackRow >= thumbStart && trackRow < thumbStart+thumbSize {
				glyph = "█"
			}
			line += styleShellScroll.Render(glyph)
		}
		lines = append(lines, line)
	}
	return styleShellBorder.Render(fill(strings.Join(lines, "\n"), innerH))
}

// shellScrollbar returns the thumb's start row and height for the current
// scroll offset. The thumb is proportional to the visible fraction, with a
// one-row minimum so even very long command output remains navigable.
func (m *Model) shellScrollbar(contentRows int) (start, size int) {
	if contentRows <= 0 || len(m.shellFlat) <= contentRows {
		return 0, 0
	}
	size = contentRows * contentRows / len(m.shellFlat)
	if size < 1 {
		size = 1
	}
	if size > contentRows {
		size = contentRows
	}
	travel := contentRows - size
	maxScroll := m.shellMaxScroll()
	if travel > 0 && maxScroll > 0 {
		start = (m.shellScroll*travel + maxScroll/2) / maxScroll
	}
	return start, size
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

// truncateToColumns cuts s to at most max grapheme columns, appending an
// ellipsis when anything is dropped. The ellipsis is charged to the budget, so
// the result never exceeds max; charging it afterwards is what let narrow roster
// rows render one column too wide.
func truncateToColumns(s string, max int) string {
	return truncateDisplay(s, max)
}
