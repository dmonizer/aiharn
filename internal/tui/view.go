package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/rivo/uniseg"

	"aiharn/internal/approval"
)

// Per-kind transcript styles. Reasoning uses a lighter 256-color gray than
// command previews, while the primary transcript kinds retain basic colors.
var (
	styleUser      = lipgloss.NewStyle().Foreground(lipgloss.Color("6")).Bold(true)
	styleAssistant = lipgloss.NewStyle()
	styleReasoning = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
	styleTool      = lipgloss.NewStyle().Foreground(lipgloss.Color("3"))
	styleError     = lipgloss.NewStyle().Foreground(lipgloss.Color("1"))
)

// styleLine renders a transcript line in its kind's color (plain passes through
// unstyled so structural and system lines keep the terminal default).
func styleLine(kind lineKind, s string) string {
	switch kind {
	case kindUser:
		return styleUser.Render(s)
	case kindAssistant:
		return styleAssistant.Render(s)
	case kindReasoning, kindReasoningStatus:
		return styleReasoning.Render(s)
	case kindTool:
		return styleTool.Render(s)
	case kindCommand:
		return styleCommand.Render(s)
	case kindError:
		return styleError.Render(s)
	default:
		return s
	}
}

// View renders the bordered panes, the input (or approval prompt), and the
// status bar on the last screen row.
func (m *Model) View() string {
	m.shellButtons = m.shellButtons[:0]
	m.rosterHits = m.rosterHits[:0]
	m.approvalLinkHit = approvalHit{}
	m.approvalCloseHit = approvalHit{}
	var b strings.Builder

	if m.shellMode == shellMaximized && m.pending == nil {
		b.WriteString(m.shellPane(m.height-1, 0))
		b.WriteString("\n")
		b.WriteString(sanitizeTerminalLine(m.statusLine()))
		return b.String()
	}

	b.WriteString(m.body())
	b.WriteString("\n")

	if m.pending != nil {
		b.WriteString(m.approvalPrompt())
	} else {
		b.WriteString(m.textarea.View())
	}
	b.WriteString("\n")
	b.WriteString(sanitizeTerminalLine(m.statusLine()))
	return b.String()
}

// body renders the transcript and subagent panes (plus an open shell pane),
// pinned to the bottom so the newest content stays visible.
func (m *Model) body() string {
	if m.approvalPopover && m.pending != nil {
		return m.approvalPopoverBody(m.rows())
	}
	if m.shellMode == shellOpen {
		shellRows := m.shellOpenRows()
		transRows := m.rows() - shellRows
		if transRows < 0 {
			transRows = 0
		}
		return m.transcriptAndSubagents(transRows) + "\n" + m.shellPane(shellRows, transRows)
	}
	return m.transcriptAndSubagents(m.rows())
}

// transcriptAndSubagents lays out the transcript beside (or above) the subagent
// list, recording the transcript's clickable command rows for mouse handling.
func (m *Model) transcriptAndSubagents(rows int) string {
	const subWidth = 32
	split := m.width >= subWidth+40
	chatWidth := m.width
	if split {
		chatWidth = m.width - subWidth - 1
	}
	if split {
		chat := m.renderChatPane(rows, chatWidth)
		roster := m.renderRosterPane(rows, chatWidth+1, subWidth, 0)
		left := strings.Split(chat, "\n")
		right := strings.Split(roster, "\n")
		joined := make([]string, rows)
		for i := range joined {
			joined[i] = left[i] + " " + right[i]
		}
		return strings.Join(joined, "\n")
	}
	rosterRows := 0
	if m.agent != nil && rows >= 6 {
		rosterRows = 4 + len(m.subagents) // border, title, agents, border
		if rosterRows > rows-3 {
			rosterRows = rows - 3 // keep a visible chat pane
		}
	}
	chatRows := rows - rosterRows
	chat := m.renderChatPane(chatRows, chatWidth)
	if rosterRows == 0 {
		return chat
	}
	roster := m.renderRosterPane(rosterRows, 0, m.width, 0)
	click := make([]int, rosterRows)
	for i := range click {
		click[i] = -1
	}
	m.clickRows = append(click, m.clickRows...)
	return roster + "\n" + chat
}

var stylePaneBorder = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))

// framePane gives each pane a one-cell, subdued border without consuming extra
// screen rows. Content and click rows begin one cell inside it.
func framePane(content []string, width, rows int) string {
	if rows <= 0 {
		return ""
	}
	if width < 4 || rows < 3 {
		return fill(strings.Join(content, "\n"), rows)
	}
	innerWidth := width - 2
	lines := make([]string, rows)
	lines[0] = stylePaneBorder.Render("╭" + strings.Repeat("─", innerWidth) + "╮")
	for i := 1; i < rows-1; i++ {
		text := ""
		if i-1 < len(content) {
			text = content[i-1]
		}
		pad := innerWidth - lipgloss.Width(text)
		if pad < 0 {
			pad = 0
		}
		lines[i] = stylePaneBorder.Render("│") + text + strings.Repeat(" ", pad) + stylePaneBorder.Render("│")
	}
	lines[rows-1] = stylePaneBorder.Render("╰" + strings.Repeat("─", innerWidth) + "╯")
	return strings.Join(lines, "\n")
}

func (m *Model) renderChatPane(rows, width int) string {
	innerRows, innerWidth := rows-2, width-2
	if rows < 3 || width < 4 {
		innerRows, innerWidth = rows, width
	}
	content, click := m.transcriptRows(innerWidth, innerRows)
	m.clickWidth = width
	m.clickRows = make([]int, rows)
	for i := range m.clickRows {
		m.clickRows[i] = -1
	}
	start := 0
	if rows >= 3 && width >= 4 {
		start = 1
	}
	for i, command := range click {
		if at := start + i; at < len(m.clickRows) {
			m.clickRows[at] = command
		}
	}
	return framePane(content, width, rows)
}

func (m *Model) renderRosterPane(rows, x, width, topY int) string {
	innerRows, innerWidth := rows-2, width-2
	if rows < 3 || width < 4 {
		innerRows, innerWidth = rows, width
	}
	innerX := x
	innerY := topY
	if rows >= 3 && width >= 4 {
		innerX++
		innerY++
	}
	content := m.renderRoster(innerRows, innerX, innerWidth, innerY)
	return framePane(strings.Split(content, "\n"), width, rows)
}

// transcriptRows returns the wrapped transcript as visual rows, bottom-pinned to
// `rows`, plus a parallel per-row clickable-command index (-1 for non-command).
func (m *Model) transcriptRows(width, rows int) ([]string, []int) {
	logical := make([]line, 0, len(m.lines))
	for _, ln := range m.lines {
		if (ln.kind == kindReasoning && !m.showReasoning) ||
			(ln.kind == kindReasoningStatus && m.showReasoning) {
			continue
		}
		logical = append(logical, ln)
	}
	if rows <= 0 {
		logical = nil
	} else if len(logical) > rows {
		logical = logical[len(logical)-rows:]
	}

	var visual []string
	var click []int
	for _, ln := range logical {
		for _, wl := range wrapLine(ln.text, width) {
			visual = append(visual, styleLine(ln.kind, wl))
			click = append(click, ln.cmd)
		}
	}
	if len(m.curText) != 0 && (m.curKind != kindReasoning || m.showReasoning) {
		for _, wl := range wrapLine(string(m.curText), width) {
			visual = append(visual, styleLine(m.curKind, wl))
			click = append(click, -1)
		}
	}
	if m.thinking && !m.showReasoning {
		status := fmt.Sprintf("thinking ... (%s)", formatThinkingElapsed(time.Since(m.thinkingStarted)))
		for _, wl := range wrapLine(status, width) {
			visual = append(visual, styleLine(kindReasoningStatus, wl))
			click = append(click, -1)
		}
	}
	if rows > 0 && len(visual) > rows {
		visual = visual[len(visual)-rows:]
		click = click[len(click)-rows:]
	}
	return visual, click
}

// rows is the number of body rows available, or 0 when unknown. The input box
// can grow up to its configured max height, so the body shrinks accordingly.
func (m *Model) rows() int {
	inputRows := m.inputBoxHeight()
	if m.pending != nil {
		inputRows = 2
	}
	h := m.height - 1 - inputRows
	if h < 0 {
		return 0
	}
	return h
}

// fill pads s to exactly n lines (truncating or appending blank lines) so the
// input line rendered below the body stays pinned to the bottom of the screen.
func fill(s string, n int) string {
	if n <= 0 {
		return s
	}
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[:n]
	} else {
		for len(lines) < n {
			lines = append(lines, "")
		}
	}
	return strings.Join(lines, "\n")
}

func (m *Model) statusLine() string {
	actions := "ask"
	if m.status.Approval == approval.ModeAllowAll.String() {
		actions = "all"
	}
	if m.gate != nil {
		if m.gate.Mode() == approval.ModeAllowAll {
			actions = "all"
		} else {
			actions = "ask"
		}
	}
	reasoningDisplay := "hidden"
	if m.showReasoning {
		reasoningDisplay = "shown"
	}
	items := []statusItem{
		{"actions allowed", actions},
		{"thinking", reasoningDisplay},
		{"view", m.focusedID},
		{"model", m.status.Model},
		{"agent", m.status.AgentType},
		{"channel", m.status.Channel},
	}
	return renderStatusItems(items, m.width)
}

type statusItem struct{ label, value string }

// renderStatusItems keeps the most important items visible on narrow screens.
// A status item is never split across columns unless even the first item is
// wider than the terminal.
func renderStatusItems(items []statusItem, width int) string {
	const separator = " │ "
	var parts []string
	for _, item := range items {
		part := item.label + " " + sanitizeTerminalLine(item.value)
		candidate := part
		if len(parts) > 0 {
			candidate = strings.Join(parts, separator) + separator + part
		}
		if width > 0 && uniseg.StringWidth(candidate) > width {
			if len(parts) == 0 {
				return truncateStatus(part, width)
			}
			break
		}
		parts = append(parts, part)
	}
	return strings.Join(parts, separator)
}

func truncateStatus(s string, width int) string {
	var b strings.Builder
	used := 0
	gr := uniseg.NewGraphemes(s)
	for gr.Next() {
		g := gr.Str()
		w := uniseg.StringWidth(g)
		if used+w > width {
			break
		}
		b.WriteString(g)
		used += w
	}
	return b.String()
}

// wrapLine hard-wraps s into visual rows no wider than width grapheme columns,
// preserving explicit newlines. Long lines never overflow the pane width, which
// keeps the fixed-height layout intact and the newest output visible.
func wrapLine(s string, width int) []string {
	if width <= 0 {
		return strings.Split(s, "\n")
	}
	var out []string
	for _, para := range strings.Split(s, "\n") {
		if para == "" {
			out = append(out, "")
			continue
		}
		var cur strings.Builder
		curW := 0
		gr := uniseg.NewGraphemes(para)
		for gr.Next() {
			g := gr.Str()
			gw := uniseg.StringWidth(g)
			if curW+gw > width && curW > 0 {
				out = append(out, cur.String())
				cur.Reset()
				curW = 0
			}
			cur.WriteString(g)
			curW += gw
		}
		out = append(out, cur.String())
	}
	return out
}
