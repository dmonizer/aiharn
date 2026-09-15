package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/rivo/uniseg"
)

// Per-kind transcript styles. ANSI basic colors work on every terminal.
var (
	styleUser      = lipgloss.NewStyle().Foreground(lipgloss.Color("6")).Bold(true)
	styleAssistant = lipgloss.NewStyle()
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

// View renders the status bar, the transcript (and, when open, the shell pane),
// and either the input line or an approval prompt.
func (m *Model) View() string {
	m.shellButtons = m.shellButtons[:0]
	var b strings.Builder
	b.WriteString(sanitizeTerminalLine(m.statusLine()))
	b.WriteString("\n")

	if m.shellMode == shellMaximized {
		b.WriteString(m.shellPane(m.height-1, 1))
		return b.String()
	}

	b.WriteString(m.body())
	b.WriteString("\n")

	if m.pending != nil {
		b.WriteString(fmt.Sprintf("approve %s? [y]es [n]o [a]llow-all", sanitizeTerminalLine(m.pending.Command)))
		if len(m.approvals) > 0 {
			b.WriteString(fmt.Sprintf(" (%d queued)", len(m.approvals)))
		}
	} else {
		b.WriteString(m.textarea.View())
	}
	return b.String()
}

// body renders the transcript and subagent panes (plus an open shell pane),
// pinned to the bottom so the newest content stays visible.
func (m *Model) body() string {
	if m.shellMode == shellOpen {
		shellRows := m.shellOpenRows()
		transRows := m.rows() - shellRows
		if transRows < 0 {
			transRows = 0
		}
		return m.transcriptAndSubagents(transRows) + "\n" + m.shellPane(shellRows, 1+transRows)
	}
	return m.transcriptAndSubagents(m.rows())
}

// transcriptAndSubagents lays out the transcript beside (or above) the subagent
// list, recording the transcript's clickable command rows for mouse handling.
func (m *Model) transcriptAndSubagents(rows int) string {
	const subWidth = 32
	split := m.width >= subWidth+40
	tw := m.width
	if split {
		tw = m.width - subWidth - 1
		if tw < 20 {
			tw = 20
		}
	}
	left, click := m.transcriptRows(tw, rows)
	m.clickRows = click
	m.clickWidth = tw

	right := m.subagentBlock(rows)
	if split {
		l := lipgloss.NewStyle().Width(tw).Render(strings.Join(left, "\n"))
		r := lipgloss.NewStyle().Width(subWidth).Render(right)
		return fill(lipgloss.JoinHorizontal(lipgloss.Top, l, r), rows)
	}
	return fill(strings.Join(left, "\n")+"\n"+right, rows)
}

// transcriptRows returns the wrapped transcript as visual rows, bottom-pinned to
// `rows`, plus a parallel per-row clickable-command index (-1 for non-command).
func (m *Model) transcriptRows(width, rows int) ([]string, []int) {
	logical := m.lines
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
	if len(m.curText) != 0 {
		for _, wl := range wrapLine(string(m.curText), width) {
			visual = append(visual, styleLine(kindAssistant, wl))
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
// can grow to a quarter of the screen, so the body shrinks accordingly.
func (m *Model) rows() int {
	h := m.height - 1 - m.inputBoxHeight()
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
