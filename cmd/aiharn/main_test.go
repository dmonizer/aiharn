package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestOpenRecorderDefaultOverrideAndDisable(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	rec, err := openRecorder(home, "", false)
	if err != nil || rec == nil {
		t.Fatalf("default recorder = %v, err=%v", rec, err)
	}
	if err := rec.Close(); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Join(home, "transcripts"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("default transcripts = %+v, err=%v", entries, err)
	}

	custom := filepath.Join(t.TempDir(), "explicit.jsonl")
	rec, err = openRecorder(home, custom, true)
	if err != nil || rec == nil {
		t.Fatalf("override recorder = %v, err=%v", rec, err)
	}
	if err := rec.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(custom); err != nil {
		t.Fatalf("custom transcript was not created: %v", err)
	}

	rec, err = openRecorder(home, "", true)
	if err != nil || rec != nil {
		t.Fatalf("disabled recorder = %v, err=%v", rec, err)
	}
	entries, err = os.ReadDir(filepath.Join(home, "transcripts"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("override changed default transcript directory: %+v, err=%v", entries, err)
	}
}
