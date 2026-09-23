package tui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
	markdown "github.com/codewandler/markdown"
	"github.com/codewandler/markdown/terminal"
	"github.com/muesli/termenv"
)

// renderMarkdownRows renders one assistant response for the transcript pane.
// Re-rendering from source is intentional: it lets markdown-go reflow lists,
// tables, quotes, and code blocks whenever Bubble Tea reports a new width.
func renderMarkdownRows(source string, width int) []string {
	ansi := terminal.AnsiOn
	if lipgloss.ColorProfile() == termenv.Ascii {
		ansi = terminal.AnsiOff
	}
	rendered, err := markdown.RenderString(source,
		terminal.WithAnsi(ansi),
		terminal.WithWrapWidth(width),
		terminal.WithCodeBlockStyle(terminal.CodeBlockStyle{
			Indent: 2, Border: true, BorderText: "│", Padding: 1,
		}),
	)
	if err != nil {
		return wrapLine(source, width)
	}

	rendered = strings.TrimSuffix(rendered, "\n")
	if rendered == "" {
		return []string{""}
	}
	rows := strings.Split(rendered, "\n")
	for i, row := range rows {
		// The renderer owns wrapping, but keep the pane invariant defensive for
		// unusually narrow widths and wide grapheme clusters.
		if width > 0 && lipgloss.Width(row) > width {
			rows[i] = truncateDisplay(row, width)
		}
	}
	return rows
}
