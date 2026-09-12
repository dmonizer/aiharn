package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// View renders the status bar, the focused transcript beside a live subagent
// list, and either the input line or an approval prompt.
func (m *Model) View() string {
	var b strings.Builder
	b.WriteString(sanitizeTerminalLine(m.statusLine()))
	b.WriteString("\n")
	b.WriteString(m.body())
	b.WriteString("\n")

	if m.pending != nil {
		b.WriteString(fmt.Sprintf("approve %s? [y]es [n]o [a]llow-all", sanitizeTerminalLine(m.pending.Command)))
		if len(m.approvals) > 0 {
			b.WriteString(fmt.Sprintf(" (%d queued)", len(m.approvals)))
		}
	} else {
		b.WriteString("> " + sanitizeTerminalLine(m.input))
	}
	return b.String()
}

// body renders the transcript and subagent panes side by side, or stacked when
// the terminal is too narrow.
func (m *Model) body() string {
	rows := m.rows()

	const subWidth = 32
	left := m.transcriptBlock(rows)
	right := m.subagentBlock(rows)

	if m.width < subWidth+40 {
		return left + "\n" + right
	}

	leftW := m.width - subWidth - 1
	if leftW < 20 {
		leftW = 20
	}
	l := lipgloss.NewStyle().Width(leftW).Render(left)
	r := lipgloss.NewStyle().Width(subWidth).Render(right)
	return lipgloss.JoinHorizontal(lipgloss.Top, l, r)
}

// rows is the number of body rows available, or 0 when unknown.
func (m *Model) rows() int {
	if m.height <= 3 {
		return 0
	}
	return m.height - 3
}

// transcriptBlock returns up to rows transcript lines (plus any partial streamed
// text), oldest first, newline-separated.
func (m *Model) transcriptBlock(rows int) string {
	lines := m.lines
	lineBudget := rows
	if lineBudget > 0 && len(m.curText) != 0 {
		lineBudget--
	}
	if lineBudget > 0 && len(lines) > lineBudget {
		lines = lines[len(lines)-lineBudget:]
	} else if rows > 0 && lineBudget == 0 {
		lines = nil
	}
	out := make([]string, 0, len(lines)+1)
	out = append(out, lines...)
	if len(m.curText) != 0 {
		out = append(out, string(m.curText))
	}
	return strings.Join(out, "\n")
}

// subagentBlock returns up to rows subagent status lines under a header.
func (m *Model) subagentBlock(rows int) string {
	lines := []string{fmt.Sprintf("subagents (%d)", len(m.subagents))}
	for _, s := range m.subagents {
		lines = append(lines, sanitizeTerminalLine(fmt.Sprintf("%s %s", s.ID, s.State)))
	}
	if rows > 0 && len(lines) > rows {
		lines = lines[:rows]
	}
	return strings.Join(lines, "\n")
}

func (m *Model) statusLine() string {
	approval := m.status.Approval
	if m.gate != nil {
		approval = m.gate.Mode().String()
	}
	return fmt.Sprintf("model %s · agent %s · channel %s · approval %s",
		m.status.Model, m.status.AgentType, m.status.Channel, approval)
}
