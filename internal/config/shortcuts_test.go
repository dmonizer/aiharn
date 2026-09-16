package config

import (
	"strings"
	"testing"
)

func TestLoadShortcutsOverrideAndDefaults(t *testing.T) {
	t.Setenv("TEST_API_KEY", "x")
	t.Setenv("TEST_ACTION_KEY", "CTRL+A")
	path := setup(t, "[shortcuts]\ntoggle_actions = \"${TEST_ACTION_KEY}\"\n"+validConfig)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Shortcuts.ToggleActions != "ctrl+a" || cfg.Shortcuts.ToggleThinking != "f10" || cfg.Shortcuts.InsertNewline != "ctrl+j" {
		t.Fatalf("shortcuts = %+v", cfg.Shortcuts)
	}
	if err := Validate(cfg, ValidateOptions{}); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestValidateShortcutsRejectsInvalidAndConflictingKeys(t *testing.T) {
	t.Setenv("TEST_API_KEY", "x")
	for _, tc := range []struct{ name, body, want string }{
		{"unmodified key", "toggle_actions = \"a\"", "shortcuts.toggle_actions"},
		{"duplicate default", "toggle_actions = \"f10\"", "conflicts with shortcuts.toggle_actions"},
		{"reserved quit", "toggle_actions = \"ctrl+c\"", "ctrl+c (quit is reserved)"},
		{"enter alias", "toggle_actions = \"ctrl+m\"", "shortcuts.toggle_actions"},
		{"tab alias", "toggle_actions = \"ctrl+i\"", "shortcuts.toggle_actions"},
		{"unsupported function key", "toggle_actions = \"f13\"", "shortcuts.toggle_actions"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := Load(setup(t, "[shortcuts]\n"+tc.body+"\n"+validConfig))
			if err != nil {
				t.Fatal(err)
			}
			err = Validate(cfg, ValidateOptions{})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Validate error = %v, want %q", err, tc.want)
			}
		})
	}
}
