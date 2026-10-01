package tui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

type rosterAction int

const (
	rosterFocus rosterAction = iota
	rosterPause
	rosterClose
)

type rosterHit struct {
	x, y, width int
	id          string
	action      rosterAction
}

func (m *Model) rosterText(id, label string, x, y int, action rosterAction) string {
	width := lipgloss.Width(label)
	m.rosterHits = append(m.rosterHits, rosterHit{x: x, y: y, width: width, id: id, action: action})
	if action == rosterFocus && m.hoverY == y && m.hoverX >= x && m.hoverX < x+width {
		return lipgloss.NewStyle().Underline(true).Render(label)
	}
	return label
}

func (m *Model) renderRoster(rows, x, width, topY int) string {
	if rows <= 0 {
		return ""
	}
	lines := []string{fmt.Sprintf("subagents (%d)", len(m.subagents))}
	if m.agent != nil && rows > 1 {
		root := truncateToColumns(m.agent.ID(), width)
		lines = append(lines, m.rosterText(m.agent.ID(), root, x, topY+1, rosterFocus))
	}
	for _, sub := range m.subagents {
		if len(lines) >= rows {
			break
		}
		y := topY + len(lines)
		state := sub.State
		if sub.Paused {
			state = "paused"
			if sub.State == "running" {
				state = "pausing"
			}
		}
		buttons := "|| x"
		if sub.Paused {
			buttons = ">  x"
		}
		stateAndButtons := " " + state + " " + buttons
		nameWidth := width - lipgloss.Width(stateAndButtons)
		if nameWidth < 1 {
			nameWidth = 1
		}
		name := truncateToColumns(sanitizeTerminalLine(sub.ID), nameWidth)
		label := m.rosterText(sub.ID, name, x, y, rosterFocus)
		prefix := label + " " + state + " "
		pauseX := x + lipgloss.Width(name) + 1 + lipgloss.Width(state) + 1
		m.rosterHits = append(m.rosterHits, rosterHit{x: pauseX, y: y, width: 2, id: sub.ID, action: rosterPause})
		m.rosterHits = append(m.rosterHits, rosterHit{x: pauseX + 3, y: y, width: 1, id: sub.ID, action: rosterClose})
		lines = append(lines, prefix+buttons)
	}
	return strings.Join(lines, "\n")
}

func (m *Model) clickRoster(x, y int) (bool, tea.Cmd) {
	for _, hit := range m.rosterHits {
		if y != hit.y || x < hit.x || x >= hit.x+hit.width {
			continue
		}
		switch hit.action {
		case rosterFocus:
			m.focusAgent(hit.id)
			if m.thinking {
				return true, tickThinking()
			}
		case rosterPause:
			sub := m.manager.Agent(hit.id)
			if sub == nil {
				return true, nil
			}
			paused := false
			for _, status := range m.subagents {
				if status.ID == hit.id {
					paused = status.Paused
					break
				}
			}
			if err := m.manager.SetSubagentPaused(m.ctx, m.agent.ID(), hit.id, !paused); err != nil {
				m.appendLine(kindError, "pause subagent: "+err.Error())
			} else {
				m.refreshSubagents()
			}
		case rosterClose:
			id := hit.id
			return true, func() tea.Msg {
				return subagentClosedMsg{gen: m.sessionGen, id: id, err: m.manager.CloseSubagent(m.ctx, m.agent.ID(), id)}
			}
		}
		return true, nil
	}
	return false, nil
}
