package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEnsureDefaultConfigCreatesValidStarterMissingOnlyAPIKey(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".aiharn")
	path := filepath.Join(home, "config.toml")

	created, err := EnsureDefaultConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Fatal("EnsureDefaultConfig did not create the config")
	}

	for _, name := range []string{path, filepath.Join(home, "prompts", "main.md")} {
		info, err := os.Stat(name)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Errorf("%s mode = %o, want 600", name, got)
		}
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load generated config: %v", err)
	}
	err = Validate(cfg, ValidateOptions{})
	if err == nil || !strings.Contains(err.Error(), `models["default"].api_key is required`) {
		t.Fatalf("Validate generated config = %v, want missing API key", err)
	}
	if strings.Count(err.Error(), "\n  - ") != 1 {
		t.Fatalf("generated config has unexpected validation errors: %v", err)
	}
}

func TestEnsureDefaultConfigDoesNotOverwriteExistingFiles(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".aiharn")
	path := filepath.Join(home, "config.toml")
	promptPath := filepath.Join(home, "prompts", "main.md")
	if err := os.MkdirAll(filepath.Dir(promptPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(promptPath, []byte("custom prompt\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("custom config\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	created, err := EnsureDefaultConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if created {
		t.Fatal("EnsureDefaultConfig reported creating an existing config")
	}
	for name, want := range map[string]string{
		path:       "custom config\n",
		promptPath: "custom prompt\n",
	} {
		got, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != want {
			t.Errorf("%s was overwritten: %q", name, got)
		}
	}
}

func TestEnsureDefaultConfigPreservesExistingPrompt(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".aiharn")
	path := filepath.Join(home, "config.toml")
	promptPath := filepath.Join(home, "prompts", "main.md")
	if err := os.MkdirAll(filepath.Dir(promptPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(promptPath, []byte("custom prompt\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	created, err := EnsureDefaultConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Fatal("EnsureDefaultConfig did not create the missing config")
	}
	got, err := os.ReadFile(promptPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "custom prompt\n" {
		t.Fatalf("existing prompt was overwritten: %q", got)
	}
}
