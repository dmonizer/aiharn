package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func TestSlashCommandsHandledLocally(t *testing.T) {
	m := newTestModel(t)

	// /help is handled locally and must not enqueue to the agent or start a turn.
	m, _ = upd(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/help")})
	m, cmd := upd(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if cmd != nil {
		t.Fatal("slash command must not start a turn")
	}
	if m.running {
		t.Fatal("slash command must not mark the model running")
	}
	if len(m.queue) != 0 {
		t.Fatalf("slash command enqueued to agent: %v", m.queue)
	}
	if len(m.lines) == 0 || !strings.Contains(m.lines[len(m.lines)-1], "/help") {
		t.Fatalf("expected help text, last line = %q", m.lines[len(m.lines)-1])
	}

	// An unknown command is also handled locally.
	m, _ = upd(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/bogus")})
	m, cmd = upd(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if cmd != nil || len(m.queue) != 0 {
		t.Fatalf("unknown command leaked to agent: cmd=%v queue=%v", cmd != nil, m.queue)
	}
	if len(m.lines) == 0 || !strings.Contains(m.lines[len(m.lines)-1], "unknown command") {
		t.Fatalf("expected unknown-command message, last line = %q", m.lines[len(m.lines)-1])
	}
}

func TestPlainInputStillEnqueues(t *testing.T) {
	m := newTestModel(t)
	m, _ = upd(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("hello")})
	m, cmd := upd(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if len(m.queue) != 0 {
		t.Fatalf("plain input should start a turn immediately, queue=%v", m.queue)
	}
	if cmd == nil || !m.running {
		t.Fatalf("plain input should start a turn: running=%v cmd=%v", m.running, cmd != nil)
	}
}
