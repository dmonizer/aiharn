package config

import (
	"fmt"
	"strconv"
	"strings"
)

// ShortcutsConfig holds configurable TUI hotkeys. Values use Bubble Tea key
// names such as "f9", "ctrl+a", and "ctrl+up".
type ShortcutsConfig struct {
	ToggleActions  string `toml:"toggle_actions"`
	ToggleThinking string `toml:"toggle_thinking"`
	CycleShell     string `toml:"cycle_shell"`
	GrowInput      string `toml:"grow_input"`
	ShrinkInput    string `toml:"shrink_input"`
	InsertNewline  string `toml:"insert_newline"`
}

func DefaultShortcuts() ShortcutsConfig {
	return ShortcutsConfig{
		ToggleActions:  "f9",
		ToggleThinking: "f10",
		CycleShell:     "ctrl+s",
		GrowInput:      "ctrl+up",
		ShrinkInput:    "ctrl+down",
		InsertNewline:  "ctrl+j",
	}
}

// WithDefaults fills omitted bindings and normalizes spelling for matching.
func (s ShortcutsConfig) WithDefaults() ShortcutsConfig {
	d := DefaultShortcuts()
	fill := func(value, fallback string) string {
		value = strings.ToLower(strings.TrimSpace(value))
		if value == "" {
			return fallback
		}
		return value
	}
	s.ToggleActions = fill(s.ToggleActions, d.ToggleActions)
	s.ToggleThinking = fill(s.ToggleThinking, d.ToggleThinking)
	s.CycleShell = fill(s.CycleShell, d.CycleShell)
	s.GrowInput = fill(s.GrowInput, d.GrowInput)
	s.ShrinkInput = fill(s.ShrinkInput, d.ShrinkInput)
	s.InsertNewline = fill(s.InsertNewline, d.InsertNewline)
	return s
}

func validateShortcuts(errs *[]string, s ShortcutsConfig) {
	s = s.WithDefaults()
	bindings := []struct{ name, key string }{
		{"toggle_actions", s.ToggleActions},
		{"toggle_thinking", s.ToggleThinking},
		{"cycle_shell", s.CycleShell},
		{"grow_input", s.GrowInput},
		{"shrink_input", s.ShrinkInput},
		{"insert_newline", s.InsertNewline},
	}
	seen := make(map[string]string, len(bindings))
	for _, binding := range bindings {
		if !validShortcutKey(binding.key) {
			*errs = append(*errs, fmt.Sprintf("shortcuts.%s %q is invalid (use f1-f12, ctrl+letter, alt+letter, or ctrl+arrow)", binding.name, binding.key))
			continue
		}
		if binding.key == "ctrl+c" {
			*errs = append(*errs, fmt.Sprintf("shortcuts.%s cannot use ctrl+c (quit is reserved)", binding.name))
			continue
		}
		if other, exists := seen[binding.key]; exists {
			*errs = append(*errs, fmt.Sprintf("shortcuts.%s conflicts with shortcuts.%s on %q", binding.name, other, binding.key))
			continue
		}
		seen[binding.key] = binding.name
	}
}

func validShortcutKey(key string) bool {
	if strings.HasPrefix(key, "f") {
		n, err := strconv.Atoi(strings.TrimPrefix(key, "f"))
		return err == nil && n >= 1 && n <= 12 && key == "f"+strconv.Itoa(n)
	}
	for _, prefix := range []string{"ctrl+", "alt+"} {
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		suffix := strings.TrimPrefix(key, prefix)
		if len(suffix) == 1 && suffix[0] >= 'a' && suffix[0] <= 'z' {
			// Bubble Tea reports Ctrl+I as tab and Ctrl+M as enter. Ctrl+H
			// is backspace in many terminals, so none are safe shortcuts.
			if prefix == "ctrl+" && (suffix == "h" || suffix == "i" || suffix == "m") {
				return false
			}
			return true
		}
		return prefix == "ctrl+" && (suffix == "up" || suffix == "down" || suffix == "left" || suffix == "right")
	}
	return false
}
