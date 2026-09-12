package tui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

// handleCommand intercepts a slash command typed on the input line so it never
// reaches the AI. It returns a tea.Cmd for future extension; no command
// currently needs to start a turn or emit a message, so it always returns nil.
func (m *Model) handleCommand(line string) tea.Cmd {
	name := ""
	if fields := strings.Fields(line); len(fields) > 0 {
		name = fields[0]
	}

	switch name {
	case "/help", "/":
		m.appendLine("available commands:")
		m.appendLine("  /help - show this help")
	default:
		m.appendLine(fmt.Sprintf("unknown command %q; type /help for help", name))
	}
	return nil
}
