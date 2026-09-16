package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/rivo/uniseg"
)

type approvalHit struct{ x, y, width int }

func (h approvalHit) contains(x, y int) bool {
	return h.width > 0 && y == h.y && x >= h.x && x < h.x+h.width
}

var (
	styleApprovalLink = lipgloss.NewStyle().Foreground(lipgloss.Color("39")).Underline(true).Bold(true)
	styleApprovalFull = lipgloss.NewStyle().Foreground(lipgloss.Color("252"))
)

// previewCommand limits the entire visible command preview, including the
// caller's three-dot ellipsis, to max terminal columns without splitting a
// grapheme cluster. The full command stays in the approval request/shell pane.
func previewCommand(s string, max int) (string, bool) {
	if max <= 0 {
		return "", s != ""
	}
	if uniseg.StringWidth(s) <= max {
		return s, false
	}
	limit := max - 3
	if limit <= 0 {
		return "", true
	}
	var b strings.Builder
	used := 0
	gr := uniseg.NewGraphemes(s)
	for gr.Next() {
		g := gr.Str()
		w := uniseg.StringWidth(g)
		if used+w > limit {
			break
		}
		b.WriteString(g)
		used += w
	}
	return b.String(), true
}

func (m *Model) approvalPrompt() string {
	command := normalizeCommand(m.pending.Command)
	limit := commandPreviewLen
	if m.width > 0 && m.width-9 < limit { // "approve " and "?"
		limit = m.width - 9
	}
	if limit < 3 {
		limit = 3
	}
	short, truncated := previewCommand(command, limit)
	first := "approve " + short
	if truncated {
		m.approvalLinkHit = approvalHit{x: uniseg.StringWidth(first), y: m.rows(), width: 3}
		link := styleApprovalLink
		if m.approvalLinkHit.contains(m.hoverX, m.hoverY) {
			link = link.Reverse(true)
		}
		first += link.Render("...")
	}
	first += "?"
	second := "[y]es [n]o [a]llow-all"
	if len(m.approvals) > 0 {
		second += fmt.Sprintf(" (%d queued)", len(m.approvals))
	}
	return first + "\n" + second
}

func (m *Model) approvalGeometry() (boxWidth, boxHeight, contentWidth, contentRows int, wrapped []string) {
	width := m.width
	if width <= 0 {
		width = 80
	}
	boxWidth = width - 4
	if boxWidth > 90 {
		boxWidth = 90
	}
	if boxWidth < 16 {
		boxWidth = width
	}
	if boxWidth < 4 {
		boxWidth = 4
	}
	contentWidth = boxWidth - 4
	if contentWidth < 1 {
		contentWidth = 1
	}
	full := strings.ReplaceAll(sanitizeTerminalText(m.pending.Command), "\t", "    ")
	wrapped = wrapLine(full, contentWidth)
	if len(wrapped) == 0 {
		wrapped = []string{""}
	}
	rows := m.rows()
	limit := rows - 2
	if limit > 20 {
		limit = 20
	}
	if limit < 3 {
		limit = rows
	}
	boxHeight = len(wrapped) + 2
	if boxHeight > limit {
		boxHeight = limit
	}
	if boxHeight < 0 {
		boxHeight = 0
	}
	contentRows = boxHeight - 2
	if contentRows < 0 {
		contentRows = 0
	}
	return
}

func (m *Model) approvalPageSize() int {
	_, _, _, n, _ := m.approvalGeometry()
	if n < 1 {
		return 1
	}
	return n
}

func (m *Model) approvalMaxScroll() int {
	_, _, _, rows, wrapped := m.approvalGeometry()
	if n := len(wrapped) - rows; n > 0 {
		return n
	}
	return 0
}

func (m *Model) scrollApproval(delta int) {
	m.approvalScroll += delta
	if m.approvalScroll < 0 {
		m.approvalScroll = 0
	}
	if max := m.approvalMaxScroll(); m.approvalScroll > max {
		m.approvalScroll = max
	}
}

// approvalPopoverBody replaces the chat body with a centered, scrollable box.
// The approval choices stay visible below it; only the [x] and Esc close it.
func (m *Model) approvalPopoverBody(rows int) string {
	if rows <= 0 {
		return ""
	}
	boxWidth, boxHeight, contentWidth, contentRows, wrapped := m.approvalGeometry()
	if boxHeight <= 0 {
		return fill("", rows)
	}
	m.scrollApproval(0)
	startY := (rows - boxHeight) / 2
	startX := (m.width - boxWidth) / 2
	if startX < 0 {
		startX = 0
	}
	label := " full command "
	if boxWidth < 22 {
		label = " cmd "
	}
	space := boxWidth - 2 - uniseg.StringWidth(label) - 3
	if space < 0 {
		space = 0
	}
	closeX := startX + 1 + uniseg.StringWidth(label) + space
	m.approvalCloseHit = approvalHit{x: closeX, y: startY, width: 3}
	closeStyle := styleApprovalLink
	if m.approvalCloseHit.contains(m.hoverX, m.hoverY) {
		closeStyle = closeStyle.Reverse(true)
	}
	top := "╭" + label + strings.Repeat("─", space) + closeStyle.Render("[x]") + "╮"
	box := []string{top}
	for i := 0; i < contentRows; i++ {
		line := ""
		if at := m.approvalScroll + i; at < len(wrapped) {
			line = wrapped[at]
		}
		padding := contentWidth - uniseg.StringWidth(line)
		if padding < 0 {
			padding = 0
		}
		box = append(box, "│ "+styleApprovalFull.Render(line)+strings.Repeat(" ", padding)+" │")
	}
	if boxHeight >= 2 {
		box = append(box, "╰"+strings.Repeat("─", boxWidth-2)+"╯")
	}
	visual := make([]string, rows)
	for i, line := range box {
		if at := startY + i; at >= 0 && at < rows {
			visual[at] = strings.Repeat(" ", startX) + line
		}
	}
	return strings.Join(visual, "\n")
}
