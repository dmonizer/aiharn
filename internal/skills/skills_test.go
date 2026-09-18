package skills

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExpandSkillsIndex(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "skills"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "skills", "index.md"), []byte("index contents"), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := Expand("before ${SKILLS_INDEX} after", dir)
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	if want := "before index contents after"; got != want {
		t.Fatalf("Expand = %q, want %q", got, want)
	}
}

func TestExpandUnknownTag(t *testing.T) {
	_, err := Expand("use ${FOO} here", t.TempDir())
	if err == nil {
		t.Fatal("expected error for unknown tag")
	}
	if !strings.Contains(err.Error(), "FOO") || !strings.Contains(err.Error(), "SKILLS_INDEX") {
		t.Fatalf("error %q does not mention FOO and SKILLS_INDEX", err)
	}
}

func TestExpandMissingIndex(t *testing.T) {
	dir := t.TempDir() // deliberately no skills/index.md

	_, err := Expand("${SKILLS_INDEX}", dir)
	if err == nil {
		t.Fatal("expected error for missing index.md")
	}
	want := filepath.Join(dir, "skills", "index.md")
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("error %q does not contain path %q", err, want)
	}
}

func TestExpandNoTags(t *testing.T) {
	// A non-existent aiharnHome must not matter when no tag is referenced.
	dir := filepath.Join(t.TempDir(), "does-not-exist")

	got, err := Expand("plain prompt with no tags", dir)
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	if got != "plain prompt with no tags" {
		t.Fatalf("Expand = %q, want unchanged input", got)
	}
}

func TestList(t *testing.T) {
	// Case A: no skills dir.
	dir := t.TempDir()
	got, err := List(dir)
	if err != nil {
		t.Fatalf("List (missing dir): %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("List (missing dir) = %v, want empty", got)
	}

	// Case B: skills with two valid skills, an index.md, and a directory
	// without SKILL.md.
	skillsDir := filepath.Join(dir, "skills")
	for _, name := range []string{"a", "b", "noskill"} {
		if err := os.MkdirAll(filepath.Join(skillsDir, name), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(skillsDir, "a", "SKILL.md"), []byte("a"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillsDir, "b", "SKILL.md"), []byte("b"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillsDir, "index.md"), []byte("index"), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err = List(dir)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("List = %v, want exactly [a b]", got)
	}
	if got[0].Name != "a" || got[0].Path != filepath.Join(skillsDir, "a") {
		t.Fatalf("List[0] = %+v, want a at %q", got[0], filepath.Join(skillsDir, "a"))
	}
	if got[1].Name != "b" || got[1].Path != filepath.Join(skillsDir, "b") {
		t.Fatalf("List[1] = %+v, want b at %q", got[1], filepath.Join(skillsDir, "b"))
	}
}

func TestInstallPrompt(t *testing.T) {
	dir := t.TempDir()
	promptsDir := filepath.Join(dir, "prompts")
	if err := os.MkdirAll(promptsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(promptsDir, "skill-install.md"), []byte("install ${URL} into ${SKILLS_DIR}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := InstallPrompt(dir, "https://example.com/x")
	if err != nil {
		t.Fatalf("InstallPrompt: %v", err)
	}
	want := "install https://example.com/x into " + filepath.Join(dir, "skills") + "\n"
	if got != want {
		t.Fatalf("InstallPrompt = %q, want %q", got, want)
	}
}

func TestInstallPromptMissingFile(t *testing.T) {
	dir := t.TempDir()
	_, err := InstallPrompt(dir, "https://example.com/x")
	if err == nil {
		t.Fatal("expected error for missing skill-install.md")
	}
	want := filepath.Join(dir, "prompts", "skill-install.md")
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("error %q does not contain path %q", err, want)
	}
}

func TestInstallPromptUnknownTag(t *testing.T) {
	dir := t.TempDir()
	promptsDir := filepath.Join(dir, "prompts")
	if err := os.MkdirAll(promptsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(promptsDir, "skill-install.md"), []byte("${BOGUS}"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := InstallPrompt(dir, "https://example.com/x")
	if err == nil {
		t.Fatal("expected error for unknown tag")
	}
	if !strings.Contains(err.Error(), "BOGUS") || !strings.Contains(err.Error(), "URL") || !strings.Contains(err.Error(), "SKILLS_DIR") {
		t.Fatalf("error %q does not mention BOGUS and supported tags URL and SKILLS_DIR", err)
	}
}
