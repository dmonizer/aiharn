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
	styleTool      = lipgloss.NewStyle().Foreground(lipgloss.Color("11")).Bold(true)
	styleError     = lipgloss.NewStyle().Foreground(lipgloss.Color("1"))
	// Agent-to-agent traffic gets its own magenta so it is never mistaken for
	// human input (cyan) or assistant output (default).
	styleAgentMessage = lipgloss.NewStyle().Foreground(lipgloss.Color("5"))
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
	case kindAgentMessage:
		return styleAgentMessage.Render(s)
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
	groupHit := make([]int, rosterRows)
	for i := range groupHit {
		groupHit[i] = -1
	}
	m.groupHitRows = append(groupHit, m.groupHitRows...)
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
		// A row must never be wider than the pane that frames it: an over-wide
		// row soft-wraps in the terminal and corrupts the renderer's cursor
		// accounting, which interleaves successive frames on the same rows.
		if lipgloss.Width(text) > innerWidth {
			text = truncateDisplay(text, innerWidth)
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
	m.groupHitRows = make([]int, rows)
	for i := range m.groupHitRows {
		m.groupHitRows[i] = -1
	}
	for i, gid := range m.chatGroupHits {
		if at := start + i; at < len(m.groupHitRows) {
			m.groupHitRows[at] = gid
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

// transcriptRows returns the wrapped transcript as a scrollable viewport of
// `rows` visual rows, plus a parallel per-row clickable-command index (-1 for
// non-command). The viewport is pinned to the newest content while chatFollow
// is set; scrolling away disables following and a return to the bottom re-arms
// it, mirroring the shell pane.
func (m *Model) transcriptRows(width, rows int) ([]string, []int) {
	if rows <= 0 {
		m.chatMaxScroll = 0
		m.chatGroupHits = nil
		return nil, nil
	}
	logical := make([]line, 0, len(m.lines))
	for _, ln := range m.lines {
		if (ln.kind == kindReasoning && !m.showReasoning) ||
			(ln.kind == kindReasoningStatus && m.showReasoning) {
			continue
		}
		logical = append(logical, ln)
	}
	if len(m.curText) != 0 && (m.curKind != kindReasoning || m.showReasoning) {
		logical = append(logical, line{text: string(m.curText), kind: m.curKind, cmd: -1})
	}
	if m.thinking && !m.showReasoning {
		logical = append(logical, line{
			text: fmt.Sprintf("thinking ... (%s)", formatThinkingElapsed(time.Since(m.thinkingStarted))),
			kind: kindReasoningStatus, cmd: -1,
		})
	}

	var visual []string
	var click []int
	var groupHit []int
	for i := 0; i < len(logical); {
		ln := logical[i]
		if ln.kind == kindAssistant {
			// Assistant deltas are stored as plain logical lines so transcript
			// history remains resizeable. Reassemble each contiguous response and
			// let markdown-go perform the Markdown-aware wrapping and styling for
			// the current pane width.
			var source strings.Builder
			for i < len(logical) && logical[i].kind == kindAssistant {
				if source.Len() > 0 {
					source.WriteByte('\n')
				}
				source.WriteString(logical[i].text)
				i++
			}
			for _, rendered := range renderMarkdownRows(source.String(), width) {
				visual = append(visual, rendered)
				click = append(click, -1)
				groupHit = append(groupHit, -1)
			}
			continue
		}
		if ln.kind == kindToolGroup && len(ln.calls) > 0 {
			calls := ln.calls
			if len(calls) == 1 {
				for _, wl := range wrapLine(toolCallText(calls[0]), width) {
					visual = append(visual, styleLine(kindTool, wl))
					click = append(click, -1)
					groupHit = append(groupHit, -1)
				}
				i++
				continue
			}
			header := fmt.Sprintf("[tool] tool calls (%d)", len(calls))
			for _, wl := range wrapLine(header, width) {
				visual = append(visual, styleLine(kindTool, wl))
				click = append(click, -1)
				groupHit = append(groupHit, ln.groupID)
			}
			if m.expandedToolGroups[ln.groupID] {
				for _, call := range calls {
					for _, wl := range wrapLine("  "+toolCallText(call), width) {
						visual = append(visual, styleLine(kindTool, wl))
						click = append(click, -1)
						groupHit = append(groupHit, -1)
					}
				}
			}
			i++
			continue
		}
		for _, wl := range wrapLine(ln.text, width) {
			visual = append(visual, styleLine(ln.kind, wl))
			click = append(click, ln.cmd)
			groupHit = append(groupHit, -1)
		}
		i++
	}

	max := len(visual) - rows
	if max < 0 {
		max = 0
	}
	m.chatMaxScroll = max
	m.followChat(max)

	start := m.chatScroll
	if start > max {
		start = max
	}
	if start < 0 {
		start = 0
	}
	end := start + rows
	if end > len(visual) {
		end = len(visual)
	}
	m.chatGroupHits = groupHit[start:end]
	return visual[start:end], click[start:end]
}

// followChat keeps the transcript viewport pinned to the newest content while
// chatFollow is set, and clamps a scrolled-away viewport to the available range.
func (m *Model) followChat(max int) {
	if m.chatFollow {
		m.chatScroll = max
		return
	}
	if m.chatScroll > max {
		m.chatScroll = max
	}
	if m.chatScroll < 0 {
		m.chatScroll = 0
	}
}

// setChatScroll sets the transcript scroll offset and updates chatFollow so the
// view follows new content again once the user scrolls back to the bottom. It
// clamps directly (rather than via followChat) so an explicit scroll-away is
// not immediately snapped back to the bottom by a still-set chatFollow flag.
func (m *Model) setChatScroll(n int) {
	m.chatScroll = n
	max := m.chatMaxScroll
	if m.chatScroll > max {
		m.chatScroll = max
	}
	if m.chatScroll < 0 {
		m.chatScroll = 0
	}
	m.chatFollow = m.chatScroll >= max
}

func (m *Model) scrollChat(delta int) {
	m.setChatScroll(m.chatScroll + delta)
}

// chatPageSize is the number of transcript rows scrolled by pgup/pgdown. It
// matches the focused chat pane's content height when the shell is closed.
func (m *Model) chatPageSize() int {
	n := m.rows() - 2
	if n < 1 {
		n = 1
	}
	return n
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
	loop := "inactive"
	if m.agent != nil && m.agent.MainLoopActive() {
		loop = "active"
	}
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
		{"loop", loop},
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
//
// Tabs are expanded first so a tab is measured as the columns a terminal will
// actually use, and each row is trimmed as a last resort in case a single
// grapheme cluster is wider than width: no returned row can exceed width.
func wrapLine(s string, width int) []string {
	if width <= 0 {
		return strings.Split(s, "\n")
	}
	s = expandTabs(s)
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
				out = append(out, truncateDisplay(cur.String(), width))
				cur.Reset()
				curW = 0
			}
			cur.WriteString(g)
			curW += gw
		}
		out = append(out, truncateDisplay(cur.String(), width))
	}
	return out
}

// truncateDisplay cuts s to at most max display columns without splitting a
// grapheme cluster, appending an ellipsis and, for styled input, a reset when
// anything is dropped: the result is never wider than max. Unlike
// uniseg.StringWidth it ignores ANSI escape sequences, which occupy no columns,
// so it is safe on text that has already been through styleLine.
func truncateDisplay(s string, max int) string {
	if max <= 0 {
		return ""
	}
	if lipgloss.Width(s) <= max {
		return s
	}
	var b strings.Builder
	used, limit := 0, max-1 // reserve one column for the ellipsis
	styled, cut := false, false
	for i := 0; i < len(s) && !cut; {
		if s[i] == 0x1b {
			end := ansiEnd(s, i)
			b.WriteString(s[i:end])
			styled = true
			i = end
			continue
		}
		j := i
		for j < len(s) && s[j] != 0x1b {
			j++
		}
		gr := uniseg.NewGraphemes(s[i:j])
		for gr.Next() {
			g := gr.Str()
			w := uniseg.StringWidth(g)
			if used+w > limit {
				cut = true
				break
			}
			b.WriteString(g)
			used += w
		}
		i = j
	}
	b.WriteString("\u2026")
	if styled {
		b.WriteString("\x1b[0m")
	}
	return b.String()
}

// ansiEnd returns the index just past the ANSI escape sequence starting at i,
// or the end of the string when the sequence is unterminated.
func ansiEnd(s string, i int) int {
	j := i + 1
	if j >= len(s) {
		return len(s)
	}
	if s[j] == ']' {
		// OSC sequences (including markdown-go's OSC 8 hyperlinks) end in
		// BEL or ST. Treat the whole sequence as zero-width so truncation never
		// displays or splits its control payload.
		for j = j + 1; j < len(s); j++ {
			if s[j] == '\a' {
				return j + 1
			}
			if s[j] == 0x1b && j+1 < len(s) && s[j+1] == '\\' {
				return j + 2
			}
		}
		return len(s)
	}
	if s[j] != '[' {
		return j + 1
	}
	j++
	for j < len(s) && (s[j] < 0x40 || s[j] > 0x7e) {
		j++
	}
	if j < len(s) {
		j++
	}
	return j
}
