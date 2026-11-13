package local

import (
	"context"
	"strings"
	"testing"

	"aiharn/internal/execution"
	"aiharn/internal/execution/fstest"
)

// TestFilesystemContract runs the shared backend contract against the local
// confined filesystem.

func TestFilesystemContract(t *testing.T) {
	fstest.Contract(t, func(t *testing.T) (execution.Filesystem, string) {
		t.Helper()
		root := t.TempDir()
		return openTestFilesystem(t, root), root
	})
}

// openTestFilesystem builds the local backend for root and fails the test when it
// cannot be created. Closing is the caller's responsibility.
func openTestFilesystem(t *testing.T, root string) execution.Filesystem {
	t.Helper()
	transport, err := NewTransport(Options{})
	if err != nil {
		t.Fatalf("NewTransport: %v", err)
	}
	fs, err := transport.NewFilesystem(context.Background(), root)
	if err != nil {
		t.Fatalf("NewFilesystem(%q): %v", root, err)
	}
	return fs
}

func TestNewFilesystemTarget(t *testing.T) {
	root := t.TempDir()
	fs := openTestFilesystem(t, root)
	t.Cleanup(func() { _ = fs.Close() })
	if !strings.HasPrefix(fs.Target(), "local:") {
		t.Fatalf("Target() = %q, want a local: prefix", fs.Target())
	}
	if !strings.HasSuffix(fs.Target(), root) {
		t.Fatalf("Target() = %q, want it to end with %q", fs.Target(), root)
	}
}

func TestNewFilesystemContextDone(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	transport, err := NewTransport(Options{})
	if err != nil {
		t.Fatalf("NewTransport: %v", err)
	}
	fs, err := transport.NewFilesystem(ctx, t.TempDir())
	if err == nil {
		_ = fs.Close()
		t.Fatal("NewFilesystem with a done context succeeded")
	}
	requireFSECode(t, err, execution.FSIOError)
}

func TestFilesystemMethodsHonourContext(t *testing.T) {
	fs := openTestFilesystem(t, t.TempDir())
	t.Cleanup(func() { _ = fs.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := fs.ReadFile(ctx, "a.txt", 16); !execution.IsFSECode(err, execution.FSIOError) {
		t.Fatalf("ReadFile with done context: %v", err)
	}
	if _, err := fs.CreateFile(ctx, "a.txt", []byte("x")); !execution.IsFSECode(err, execution.FSIOError) {
		t.Fatalf("CreateFile with done context: %v", err)
	}
	if _, err := fs.ReplaceFile(ctx, "a.txt", []byte("x"), execution.FileVersion{}); !execution.IsFSECode(err, execution.FSIOError) {
		t.Fatalf("ReplaceFile with done context: %v", err)
	}
}

func TestNewFilesystemRejectsInvalidRoot(t *testing.T) {
	transport, err := NewTransport(Options{})
	if err != nil {
		t.Fatalf("NewTransport: %v", err)
	}
	for name, root := range map[string]string{
		"relative": "relative/root",
		"empty":    "",
		"missing":  "/nonexistent/aiharn-test-root",
	} {
		t.Run(name, func(t *testing.T) {
			fs, err := transport.NewFilesystem(context.Background(), root)
			if err == nil {
				_ = fs.Close()
				t.Fatalf("NewFilesystem(%q) succeeded, want an error", root)
			}
			if execution.FSECode(err) == "" {
				t.Fatalf("NewFilesystem(%q) error carries no code: %v", root, err)
			}
		})
	}
}

func TestFilesystemCloseIsIdempotent(t *testing.T) {
	fs := openTestFilesystem(t, t.TempDir())
	if err := fs.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := fs.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if _, err := fs.ReadFile(context.Background(), "a.txt", 16); !execution.IsFSECode(err, execution.FSIOError) {
		t.Fatalf("ReadFile after Close: %v", err)
	}
}

func requireFSECode(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error with code %q, got nil", want)
	}
	if code := execution.FSECode(err); code != want {
		t.Fatalf("code = %q, want %q (err: %v)", code, want, err)
	}
}
