package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// Per-kind transcript styles. ANSI basic colors work on every terminal.
var (
	styleUser      = lipgloss.NewStyle().Foreground(lipgloss.Color("6")).Bold(true)
	styleAssistant = lipgloss.NewStyle()
	styleTool      = lipgloss.NewStyle().Foreground(lipgloss.Color("3"))
	styleError     = lipgloss.NewStyle().Foreground(lipgloss.Color("1"))
	styleApproval  = lipgloss.NewStyle().Foreground(lipgloss.Color("5"))
)

// styleLine renders a transcript line in its kind's color (plain passes through
// unstyled so structural and system lines keep the terminal default).
func styleLine(kind lineKind, s string) string {
	switch kind {
	case kindUser:
		return styleUser.Render(s)
	case kindAssistant:
		return styleAssistant.Render(s)
	case kindTool:
		return styleTool.Render(s)
	case kindError:
		return styleError.Render(s)
	case kindApproval:
		return styleApproval.Render(s)
	default:
		return s
	}
}

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
		return fill(left+"\n"+right, rows)
	}

	leftW := m.width - subWidth - 1
	if leftW < 20 {
		leftW = 20
	}
	l := lipgloss.NewStyle().Width(leftW).Render(left)
	r := lipgloss.NewStyle().Width(subWidth).Render(right)
	return fill(lipgloss.JoinHorizontal(lipgloss.Top, l, r), rows)
}

// rows is the number of body rows available, or 0 when unknown.
func (m *Model) rows() int {
	if m.height <= 2 {
		return 0
	}
	return m.height - 2
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
	for _, ln := range lines {
		out = append(out, styleLine(ln.kind, ln.text))
	}
	if len(m.curText) != 0 {
		out = append(out, styleLine(kindAssistant, string(m.curText)))
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
