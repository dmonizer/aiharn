package tui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

// handleCommand intercepts a slash command typed on the input line so it never
// reaches the AI. It returns a tea.Cmd; only /quit and /exit need one.
func (m *Model) handleCommand(line string) tea.Cmd {
	name := ""
	if fields := strings.Fields(line); len(fields) > 0 {
		name = fields[0]
	}

	switch name {
	case "/quit", "/exit":
		m.cancel()
		return tea.Quit
	case "/help", "/":
		m.appendLine(kindPlain, "available commands:")
		m.appendLine(kindPlain, "  /help - show this help")
		m.appendLine(kindPlain, "  /quit - quit")
		m.appendLine(kindPlain, "keys:")
		m.appendLine(kindPlain, "  up/down - previous prompts")
		m.appendLine(kindPlain, "  esc (including ctrl+esc) - stop all requests while active; double esc clears input while idle")
		m.appendLine(kindPlain, "  "+m.shortcuts.ToggleActions+" - toggle actions allowed between ask and all (existing prompts still need a decision)")
		m.appendLine(kindPlain, "  "+m.shortcuts.ToggleThinking+" - show/hide streamed thinking summaries")
		m.appendLine(kindPlain, "  pgup/pgdown or mouse wheel over input - scroll textarea")
		m.appendLine(kindPlain, "  "+m.shortcuts.CycleShell+" - toggle shell view (closed/open/maximized)")
		m.appendLine(kindPlain, "  "+m.shortcuts.GrowInput+"/"+m.shortcuts.ShrinkInput+" - grow/shrink input")
		m.appendLine(kindPlain, "  "+m.shortcuts.InsertNewline+" - insert newline")
		m.appendLine(kindPlain, "  click a command - open shell view focused on it")
		m.appendLine(kindPlain, "  click approval ... - inspect the full command; Esc or [x] closes the preview")
		m.appendLine(kindPlain, "  click an agent in the roster - switch its chat and terminal; || pauses queued tasks, x closes")
	default:
		m.appendLine(kindPlain, fmt.Sprintf("unknown command %q; type /help for help", name))
	}
	return nil
}
