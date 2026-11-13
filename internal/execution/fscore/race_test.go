package fscore_test

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"aiharn/internal/execution"
)

// raceIterations is how many reads each confinement race performs while the
// attacker keeps rearranging the tree underneath it.
const raceIterations = 200

// racedReadCodes are the failures a read may legitimately report while an
// attacker rearranges the tree under it.
var racedReadCodes = map[string]bool{
	execution.FSParentNotFound:    true,
	execution.FSNotFound:          true,
	execution.FSSymlinkNotAllowed: true,
	execution.FSStaleRead:         true,
	execution.FSNotRegularFile:    true,
}

// TestReadSurvivesParentSwap races a reader against an attacker that repeatedly
// moves a parent directory out of the root and replaces it with a symlink to an
// outside directory. A read may fail, but it must never return outside content.
func TestReadSurvivesParentSwap(t *testing.T) {
	service, root := openService(t)
	const inside = "inside\n"
	const secret = "TOP-SECRET\n"

	outside := t.TempDir()
	stash := t.TempDir()
	parent := filepath.Join(root, "dir")
	hidden := filepath.Join(stash, "dir")
	if err := os.Mkdir(parent, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(parent, "target.txt"), []byte(inside), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "target.txt"), []byte(secret), 0o644); err != nil {
		t.Fatal(err)
	}

	attack := func() {
		if err := os.Rename(parent, hidden); err != nil {
			return
		}
		if err := os.Symlink(outside, parent); err != nil {
			_ = os.Rename(hidden, parent)
			return
		}
		_ = os.Remove(parent)
		_ = os.Rename(hidden, parent)
	}
	runConfinementRace(t, func() (execution.FileSnapshot, error) {
		return service.Read("dir/target.txt", 4096)
	}, attack, inside)
}

// TestReadSurvivesLeafSwap races a reader against an attacker that replaces the
// leaf between the stat that validates it and the open that reads it, pointing
// the name at a file outside the root. The reader must never follow it.
func TestReadSurvivesLeafSwap(t *testing.T) {
	service, root := openService(t)
	const inside = "inside\n"
	const secret = "TOP-SECRET\n"

	outsideFile := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outsideFile, []byte(secret), 0o644); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(root, "source.txt")
	leaf := filepath.Join(root, "target.txt")
	if err := os.WriteFile(source, []byte(inside), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(source, leaf); err != nil {
		t.Skipf("hard link unsupported: %v", err)
	}

	attack := func() {
		if err := os.Remove(leaf); err != nil {
			return
		}
		if err := os.Symlink(outsideFile, leaf); err != nil {
			return
		}
		if err := os.Remove(leaf); err != nil {
			return
		}
		_ = os.Link(source, leaf)
	}
	runConfinementRace(t, func() (execution.FileSnapshot, error) {
		return service.Read("target.txt", 4096)
	}, attack, inside)
}

// runConfinementRace keeps calling attack until raceIterations reads are done and
// fails on any read that escapes the root or reports an unexpected failure.
func runConfinementRace(t *testing.T, read func() (execution.FileSnapshot, error), attack func(), allowed ...string) {
	t.Helper()
	stop := make(chan struct{})
	var attacker sync.WaitGroup
	attacker.Add(1)
	go func() {
		defer attacker.Done()
		for {
			select {
			case <-stop:
				return
			default:
				attack()
			}
		}
	}()
	defer func() {
		close(stop)
		attacker.Wait()
	}()

	for i := 0; i < raceIterations; i++ {
		snapshot, err := read()
		if err != nil {
			requireRacedError(t, err)
			continue
		}
		if !isAllowedContent(string(snapshot.Content), allowed) {
			t.Fatalf("read returned content %q, which is not confined to the root", snapshot.Content)
		}
	}
}

// requireRacedError fails unless err is a filesystem error with a code a raced
// read may produce.
func requireRacedError(t *testing.T, err error) {
	t.Helper()
	var fsErr *execution.FSError
	if !errors.As(err, &fsErr) {
		t.Fatalf("raced read returned a non-filesystem error: %v", err)
	}
	if !racedReadCodes[fsErr.Code] {
		t.Fatalf("raced read returned unexpected code %q: %v", fsErr.Code, err)
	}
}

// isAllowedContent reports whether content is one of the accepted readings.
func isAllowedContent(content string, allowed []string) bool {
	for _, candidate := range allowed {
		if content == candidate {
			return true
		}
	}
	return false
}
