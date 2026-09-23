package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
)

func TestMarkdownRendererFormatsAssistantText(t *testing.T) {
	previous := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.Ascii)
	t.Cleanup(func() { lipgloss.SetColorProfile(previous) })

	rows := renderMarkdownRows("# Heading\n\n- one\n- two\n\n**bold** and `code`", 60)
	got := strings.Join(rows, "\n")
	for _, raw := range []string{"# Heading", "**bold**", "`code`"} {
		if strings.Contains(got, raw) {
			t.Fatalf("Markdown syntax %q was not rendered: %q", raw, got)
		}
	}
	for _, rendered := range []string{"Heading", "one", "two", "bold", "code"} {
		if !strings.Contains(got, rendered) {
			t.Fatalf("rendered text %q missing from %q", rendered, got)
		}
	}
}

func TestMarkdownRendererWrapsToPaneWidth(t *testing.T) {
	previous := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.Ascii)
	t.Cleanup(func() { lipgloss.SetColorProfile(previous) })

	for _, row := range renderMarkdownRows(strings.Repeat("word ", 20), 18) {
		if width := lipgloss.Width(row); width > 18 {
			t.Fatalf("rendered row width = %d, want <= 18: %q", width, row)
		}
	}
}

func TestMarkdownRendererKeepsCodeIndent(t *testing.T) {
	previous := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.Ascii)
	t.Cleanup(func() { lipgloss.SetColorProfile(previous) })

	rows := renderMarkdownRows("        INDENTMARK", 40)
	if len(rows) == 0 || !strings.Contains(rows[0], "  INDENTMARK") {
		t.Fatalf("indented code lost visible indentation: %q", rows)
	}
}

func TestMarkdownTranscriptKeepsCodeIndent(t *testing.T) {
	previous := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.Ascii)
	t.Cleanup(func() { lipgloss.SetColorProfile(previous) })

	m := newTestModel(t)
	m.width, m.height = 20, 24
	m.resizeInput()
	m.appendLine(kindAssistant, "\t\tINDENTMARK")
	if got := m.lines[len(m.lines)-1].text; !strings.HasPrefix(got, "        ") {
		t.Fatalf("sanitized transcript line lost tab indentation: %q", got)
	}
	if view := m.View(); !strings.Contains(view, "  INDENTMARK") {
		t.Fatalf("rendered transcript lost visible code indentation: %q", view)
	}
}
