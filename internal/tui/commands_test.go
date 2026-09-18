package tui

import (
	"os"
	"path/filepath"
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
	found := false
	for _, ln := range m.lines {
		if strings.Contains(ln.text, "/help") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected help text in transcript: %+v", m.lines)
	}

	// An unknown command is also handled locally.
	m, _ = upd(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/bogus")})
	m, cmd = upd(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if cmd != nil || len(m.queue) != 0 {
		t.Fatalf("unknown command leaked to agent: cmd=%v queue=%v", cmd != nil, m.queue)
	}
	if len(m.lines) == 0 || !strings.Contains(m.lines[len(m.lines)-1].text, "unknown command") {
		t.Fatalf("expected unknown-command message, last line = %q", m.lines[len(m.lines)-1].text)
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

func TestSkillCommandListsSkills(t *testing.T) {
	m := newTestModel(t)
	home := t.TempDir()
	m.aiharnHome = home
	for _, name := range []string{"a", "b"} {
		path := filepath.Join(home, "skills", name, "SKILL.md")
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	m, _ = upd(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/skill")})
	m, cmd := upd(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if cmd != nil {
		t.Fatal("list skills must not start a turn")
	}
	if m.running {
		t.Fatal("list skills must not mark the model running")
	}
	if len(m.queue) != 0 {
		t.Fatalf("list skills enqueued to agent: %v", m.queue)
	}

	foundHeader, foundA, foundB := false, false, false
	for _, ln := range m.lines {
		if strings.Contains(ln.text, "installed skills:") {
			foundHeader = true
		}
		if strings.Contains(ln.text, "  a") {
			foundA = true
		}
		if strings.Contains(ln.text, "  b") {
			foundB = true
		}
	}
	if !foundHeader || !foundA || !foundB {
		t.Fatalf("expected installed skills a and b, got lines: %+v", m.lines)
	}
}

func TestSkillCommandInstall(t *testing.T) {
	m := newTestModel(t)
	home := t.TempDir()
	m.aiharnHome = home
	if err := os.MkdirAll(filepath.Join(home, "prompts"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "prompts", "skill-install.md"), []byte("fetch ${URL} -> ${SKILLS_DIR}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	m, _ = upd(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/skill install https://example.com/s.git")})
	m, cmd := upd(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil || !m.running {
		t.Fatalf("install should start a turn: running=%v cmd=%v", m.running, cmd != nil)
	}
	if len(m.queue) != 0 {
		t.Fatalf("queue after install = %v, want popped by nextTurn", m.queue)
	}

	want := "> fetch https://example.com/s.git -> " + filepath.Join(home, "skills")
	found := false
	for _, ln := range m.lines {
		if strings.Contains(ln.text, want) {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected expanded install prompt %q in lines: %+v", want, m.lines)
	}
}

func TestSkillCommandInstallAlias(t *testing.T) {
	m := newTestModel(t)
	home := t.TempDir()
	m.aiharnHome = home
	if err := os.MkdirAll(filepath.Join(home, "prompts"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "prompts", "skill-install.md"), []byte("fetch ${URL} -> ${SKILLS_DIR}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	m, _ = upd(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/skill i https://example.com/x")})
	m, cmd := upd(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil || !m.running {
		t.Fatalf("install alias should start a turn: running=%v cmd=%v", m.running, cmd != nil)
	}
	if len(m.queue) != 0 {
		t.Fatalf("queue after install = %v, want popped by nextTurn", m.queue)
	}

	want := "> fetch https://example.com/x -> " + filepath.Join(home, "skills")
	found := false
	for _, ln := range m.lines {
		if strings.Contains(ln.text, want) {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected expanded install prompt %q in lines: %+v", want, m.lines)
	}
}

func TestSkillCommandUsage(t *testing.T) {
	m := newTestModel(t)
	m, _ = upd(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/skill install")})
	m, cmd := upd(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if cmd != nil {
		t.Fatal("usage error must not start a turn")
	}
	if m.running {
		t.Fatal("usage error must not mark the model running")
	}
	if len(m.lines) == 0 || !strings.Contains(m.lines[len(m.lines)-1].text, "usage:") {
		t.Fatalf("expected usage message, last line = %q", m.lines[len(m.lines)-1].text)
	}
}

func TestSkillCommandPluralAlias(t *testing.T) {
	m := newTestModel(t)
	home := t.TempDir()
	m.aiharnHome = home
	path := filepath.Join(home, "skills", "a", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("a"), 0o600); err != nil {
		t.Fatal(err)
	}

	m, _ = upd(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/skills")})
	m, cmd := upd(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if cmd != nil || m.running || len(m.queue) != 0 {
		t.Fatalf("/skills leaked a turn: running=%v cmd=%v queue=%v", m.running, cmd != nil, m.queue)
	}
	header, a := false, false
	for _, ln := range m.lines {
		if strings.Contains(ln.text, "installed skills:") {
			header = true
		}
		if strings.Contains(ln.text, "  a") {
			a = true
		}
	}
	if !header || !a {
		t.Fatalf("expected /skills to list installed skills: %+v", m.lines)
	}
}
