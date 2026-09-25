package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"aiharn/internal/approval"
	"aiharn/internal/config"
)

func TestConfiguredApprovalShortcutReplacesF9(t *testing.T) {
	base := newTestModel(t)
	m := New(base.manager, base.agent, base.gate, Status{
		Model: "m", AgentType: "main", Channel: "devbox", Approval: "ask",
		Shortcuts: config.ShortcutsConfig{ToggleActions: "ctrl+a"},
	})
	m, _ = upd(t, m, tea.KeyMsg{Type: tea.KeyF9})
	if m.gate.Mode() != approval.ModeAsk {
		t.Fatalf("F9 retained its old binding: %v", m.gate.Mode())
	}
	m, _ = upd(t, m, tea.KeyMsg{Type: tea.KeyCtrlA})
	if m.gate.Mode() != approval.ModeAllowAll {
		t.Fatalf("ctrl+a did not toggle approval: %v", m.gate.Mode())
	}
	m.handleCommand("/help")
	found := false
	for _, line := range m.lines {
		if strings.Contains(line.text, "ctrl+a - toggle actions allowed") {
			found = true
		}
	}
	if !found {
		t.Fatal("/help did not show configured shortcut")
	}
}

func TestHelpListsToggleLoop(t *testing.T) {
	m := newTestModel(t)
	m.handleCommand("/help")
	found := false
	for _, line := range m.lines {
		if strings.Contains(line.text, "ctrl+l - toggle main loop") {
			found = true
		}
	}
	if !found {
		t.Fatal("/help did not list the toggle loop shortcut")
	}
}

func TestConfiguredInsertNewlineShortcut(t *testing.T) {
	base := newTestModel(t)
	m := New(base.manager, base.agent, base.gate, Status{
		Model: "m", AgentType: "main", Channel: "devbox", Approval: "ask",
		Shortcuts: config.ShortcutsConfig{InsertNewline: "ctrl+x"},
	})
	m, _ = upd(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("first")})
	m, _ = upd(t, m, tea.KeyMsg{Type: tea.KeyCtrlX})
	m, _ = upd(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("second")})
	if got := m.textarea.Value(); got != "first\nsecond" {
		t.Fatalf("textarea = %q, want configured newline", got)
	}
}
