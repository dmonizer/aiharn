package main

import (
	"os"
	"path/filepath"
	"testing"

	"aiharn/internal/sessions"
)

// transcriptFiles returns the files in <home>/transcripts.
func transcriptFiles(t *testing.T, home string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(home, "transcripts"))
	if os.IsNotExist(err) {
		// No per-session transcripts were written at all.
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

func TestTranscriptFactoryDisabled(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	factory, closeAll, err := transcriptFactory(home, "", true)
	if err != nil {
		t.Fatal(err)
	}
	defer closeAll()

	// A nil factory leaves app.Options.NewTranscript unset, which the session
	// layer reads as "this session keeps no transcript". Returning a factory
	// that yields nil instead would make NewSessionManager fail at startup.
	if factory != nil {
		t.Fatal("disabled transcripts returned a factory")
	}
	if _, err := os.Stat(filepath.Join(home, "transcripts")); !os.IsNotExist(err) {
		t.Fatalf("disabled transcripts still created a directory: %v", err)
	}
}

func TestTranscriptFactorySharedPath(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	custom := filepath.Join(t.TempDir(), "explicit.jsonl")
	factory, closeAll, err := transcriptFactory(home, custom, true)
	if err != nil {
		t.Fatal(err)
	}

	first, err := factory("default", "Default")
	if err != nil || first == nil {
		t.Fatalf("first shared transcript = %v, err=%v", first, err)
	}
	second, err := factory("session-1", "Session 1")
	if err != nil || second == nil {
		t.Fatalf("second shared transcript = %v, err=%v", second, err)
	}
	// An explicit path pins one file for every session, so the factory must
	// hand out the same recorder rather than opening the file twice.
	if first != second {
		t.Fatal("explicit --log did not share one recorder between sessions")
	}
	// The shared recorder takes the header of whichever session writes first.
	first.SetMeta(sessions.TranscriptMeta{ID: "default", Name: "Default"})
	closeAll()
	if _, err := os.Stat(custom); err != nil {
		t.Fatalf("explicit transcript was not created: %v", err)
	}
	if names := transcriptFiles(t, home); len(names) != 0 {
		t.Fatalf("explicit path also wrote per-session transcripts: %v", names)
	}
}

func TestTranscriptFactoryPerSession(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	factory, closeAll, err := transcriptFactory(home, "", false)
	if err != nil {
		t.Fatal(err)
	}
	defer closeAll()

	first, err := factory("default", "Default")
	if err != nil || first == nil {
		t.Fatalf("default transcript = %v, err=%v", first, err)
	}
	second, err := factory("session-1", "Session 1")
	if err != nil || second == nil {
		t.Fatalf("second transcript = %v, err=%v", second, err)
	}
	if first == second {
		t.Fatal("per-session transcripts share one recorder")
	}
	// Each session owns its file, so the factory's close function must not
	// close them: the sessions do that themselves.
	first.Close()
	second.Close()

	names := transcriptFiles(t, home)
	if len(names) != 2 {
		t.Fatalf("per-session transcripts = %v, want 2", names)
	}
	for _, name := range names {
		info, err := os.Stat(filepath.Join(home, "transcripts", name))
		if err != nil {
			t.Fatal(err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Fatalf("transcript %s mode = %o, want 600", name, perm)
		}
	}
}

func TestSessionLimit(t *testing.T) {
	for _, tc := range []struct{ configured, want int }{
		{0, 8}, {-1, 8}, {1, 1}, {16, 16},
	} {
		if got := sessionLimit(tc.configured); got != tc.want {
			t.Fatalf("sessionLimit(%d) = %d, want %d", tc.configured, got, tc.want)
		}
	}
}
