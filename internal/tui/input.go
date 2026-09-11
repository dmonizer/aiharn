package tui

import (
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
