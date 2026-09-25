package tui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"aiharn/internal/skills"
)

// handleCommand intercepts a slash command typed on the input line so it never
// reaches the AI. It returns a tea.Cmd; only /quit and /exit need one.
func (m *Model) handleCommand(line string) tea.Cmd {
	fields := strings.Fields(line)
	name := ""
	if len(fields) > 0 {
		name = fields[0]
	}

	switch name {
	case "/quit", "/exit":
		m.cancel()
		return tea.Quit
	case "/skill", "/skills":
		return m.handleSkill(fields)
	case "/help", "/":
		m.appendLine(kindPlain, "available commands:")
		m.appendLine(kindPlain, "  /help - show this help")
		m.appendLine(kindPlain, "  /quit - quit")
		m.appendLine(kindPlain, "  /skill (or /skills) - list installed skills; /skill install <url> to install one")
		m.appendLine(kindPlain, "keys:")
		m.appendLine(kindPlain, "  up/down - previous prompts")
		m.appendLine(kindPlain, "  esc (including ctrl+esc) - stop all requests while active; double esc clears input while idle")
		m.appendLine(kindPlain, "  "+m.shortcuts.ToggleActions+" - toggle actions allowed between ask and all (existing prompts still need a decision)")
		m.appendLine(kindPlain, "  "+m.shortcuts.ToggleLoop+" - toggle main loop (auto-process agent messages)")
		m.appendLine(kindPlain, "  "+m.shortcuts.ToggleThinking+" - show/hide streamed thinking summaries")
		m.appendLine(kindPlain, "  pgup/pgdown - scroll the transcript when it overflows, otherwise the input")
		m.appendLine(kindPlain, "  "+m.shortcuts.CycleShell+" - toggle shell view (closed/open/maximized)")
		m.appendLine(kindPlain, "  "+m.shortcuts.GrowInput+"/"+m.shortcuts.ShrinkInput+" - grow/shrink input")
		m.appendLine(kindPlain, "  "+m.shortcuts.InsertNewline+" - insert newline")
		m.appendLine(kindPlain, "  click a command - open shell view focused on it")
		m.appendLine(kindPlain, "  click tool calls - expand/collapse consecutive tool calls")
		m.appendLine(kindPlain, "  click approval ... - inspect the full command; Esc or [x] closes the preview")
		m.appendLine(kindPlain, "  click an agent in the roster - switch its chat and terminal; || pauses queued tasks, x closes")
		m.appendLine(kindPlain, "  transcript ↑/↓ = agent-to-agent message (↑ up to an ancestor, ↓ down to a descendant; (pending) = still queued)")
	default:
		m.appendLine(kindPlain, fmt.Sprintf("unknown command %q; type /help for help", name))
	}
	return nil
}

// handleSkill implements the /skill slash command. With no arguments it lists
// installed skills; "install" or "i" followed by a URL builds the install
// prompt and sends it to the focused agent.
func (m *Model) handleSkill(fields []string) tea.Cmd {
	if len(fields) == 1 {
		m.listSkills()
		return nil
	}
	if len(fields) >= 3 && (fields[1] == "install" || fields[1] == "i") {
		url := fields[2]
		return m.installSkill(url)
	}
	m.appendLine(kindError, "usage: /skill [install|i <url>]")
	return nil
}

// listSkills prints the installed skills under the aiharn home directory.
func (m *Model) listSkills() {
	if m.aiharnHome == "" {
		m.appendLine(kindError, "skills: aiharn home is not set")
		return
	}
	list, err := skills.List(m.aiharnHome)
	if err != nil {
		m.appendLine(kindError, "list skills: "+err.Error())
		return
	}
	if len(list) == 0 {
		m.appendLine(kindPlain, "no installed skills")
		return
	}
	m.appendLine(kindPlain, "installed skills:")
	for _, s := range list {
		m.appendLine(kindPlain, "  "+s.Name)
	}
}

// installSkill builds the skill install prompt and sends it to the focused
// agent.
func (m *Model) installSkill(url string) tea.Cmd {
	if m.aiharnHome == "" {
		m.appendLine(kindError, "skills: aiharn home is not set")
		return nil
	}
	prompt, err := skills.InstallPrompt(m.aiharnHome, url)
	if err != nil {
		m.appendLine(kindError, "install skill: "+err.Error())
		return nil
	}
	m.appendLine(kindPlain, "installing skill from "+url)
	return m.sendToCurrentAgent(prompt)
}
