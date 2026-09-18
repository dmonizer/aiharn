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
