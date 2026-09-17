package tui

import (
	"context"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"aiharn/internal/llm"
)

func TestWidePaneBordersAndBottomStatus(t *testing.T) {
	m := newTestModel(t)
	m.width, m.height = 120, 14
	m.resizeInput()
	lines := strings.Split(m.View(), "\n")
	if len(lines) != m.height {
		t.Fatalf("view has %d rows, want %d", len(lines), m.height)
	}
	if strings.Count(lines[0], "╭") != 2 || strings.Count(lines[m.rows()-1], "╰") != 2 {
		t.Fatalf("chat and roster borders missing: top=%q bottom=%q", lines[0], lines[m.rows()-1])
	}
	for y := 0; y < m.rows(); y++ {
		if got := lipgloss.Width(lines[y]); got != m.width {
			t.Fatalf("body row %d width=%d, want %d: %q", y, got, m.width, lines[y])
		}
	}
	if !strings.Contains(lines[m.height-2], "> ") || lines[m.height-1] != m.statusLine() {
		t.Fatalf("input/status order wrong: %q / %q", lines[m.height-2], lines[m.height-1])
	}
}

func TestStackedPaneBordersAndRosterClick(t *testing.T) {
	m := newRosterModel(t)
	id, err := m.manager.SpawnSubagent(context.Background(), "main", "coder", "", "task")
	if err != nil {
		t.Fatal(err)
	}
	m.refreshSubagents()
	m.width, m.height = 65, 18
	m.resizeInput()
	lines := strings.Split(m.View(), "\n")
	if len(lines) != m.height || !strings.Contains(lines[0], "╭") {
		t.Fatalf("stacked layout missing roster border: %d lines", len(lines))
	}
	if strings.Count(strings.Join(lines[:m.rows()], "\n"), "╭") != 2 || strings.Count(strings.Join(lines[:m.rows()], "\n"), "╰") != 2 {
		t.Fatal("stacked roster and chat both need borders")
	}
	var hit rosterHit
	for _, candidate := range m.rosterHits {
		if candidate.id == id && candidate.action == rosterFocus {
			hit = candidate
		}
	}
	if hit.width == 0 {
		t.Fatal("subagent link missing from stacked roster")
	}
	m, _ = upd(t, m, tea.MouseMsg{X: hit.x, Y: hit.y, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress})
	if m.focusedID != id {
		t.Fatalf("stacked roster click focused %q, want %q", m.focusedID, id)
	}
}

func TestChatBorderDoesNotOpenCommand(t *testing.T) {
	m := newTestModel(t)
	m.width, m.height = 120, 14
	m.resizeInput()
	m.appendToolCall(llm.Item{CallID: "c", Name: "execute_command", Args: `{"command":"pwd"}`})
	m.View()
	y := -1
	for i, command := range m.clickRows {
		if command == 0 {
			y = i
			break
		}
	}
	if y < 0 {
		t.Fatal("clickable command not rendered")
	}
	m.clickTranscript(0, y)
	if m.shellMode != shellClosed {
		t.Fatal("border click opened a command")
	}
	m.clickTranscript(1, y)
	if m.shellMode != shellOpen {
		t.Fatal("chat content click did not open command")
	}
}
