package tui

import (
	"strings"
	"unicode"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"
)

// truncateLastRune removes the last UTF-8 rune from s.
func truncateLastRune(s string) string {
	if s == "" {
		return s
	}
	_, size := utf8.DecodeLastRuneInString(s)
	return s[:len(s)-size]
}

// isPrintable reports whether the key contributes input characters (as opposed
// to a named control key).
func isPrintable(k tea.KeyMsg) bool {
	return k.Type == tea.KeyRunes || k.Type == tea.KeySpace
}

// sanitizeTerminalText removes terminal control sequences from untrusted model,
// tool, and remote-command text while preserving normal whitespace. In
// particular ESC and C1 controls must never reach the user's terminal.
func sanitizeTerminalText(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' || unicode.IsPrint(r) {
			return r
		}
		return -1
	}, s)
}

func sanitizeTerminalLine(s string) string {
	s = sanitizeTerminalText(s)
	return strings.NewReplacer("\n", " ", "\r", " ", "\t", " ").Replace(s)
}
