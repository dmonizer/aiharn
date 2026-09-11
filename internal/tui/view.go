package tui

import (
	"fmt"
	"strings"
)

// View renders the status bar, transcript, and either the input line or an
// approval prompt.
func (m *Model) View() string {
	var b strings.Builder

	b.WriteString(m.statusLine())
	b.WriteString("\n")

	visible := m.lines
	if m.height > 0 {
		if max := m.height - 3; max >= 0 && len(visible) > max {
			visible = visible[len(visible)-max:]
		}
	}
	for _, l := range visible {
		b.WriteString(l)
		b.WriteString("\n")
	}
	if m.curText != "" {
		b.WriteString(m.curText)
	}

	if m.pending != nil {
		b.WriteString(fmt.Sprintf("\napprove %s? [y]es [n]o [a]llow-all", m.pending.Command))
	} else {
		b.WriteString("\n> " + m.input)
	}
	return b.String()
}

func (m *Model) statusLine() string {
	approval := m.status.Approval
	if m.gate != nil {
		approval = m.gate.Mode().String()
	}
	return fmt.Sprintf("model %s · agent %s · channel %s · approval %s",
		m.status.Model, m.status.AgentType, m.status.Channel, approval)
}
