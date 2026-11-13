package ssh_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"aiharn/internal/execution"
	"aiharn/internal/execution/fscore"
	"aiharn/internal/execution/fshelper"
	"aiharn/internal/execution/fstest"
	execssh "aiharn/internal/execution/ssh"
	"aiharn/internal/execution/ssh/harness"
)

// Environment guards that turn this test binary into the remote helper and let
// a test observe a mutation whose reply never arrives.
const (
	testFSHelperEnv    = "AIHARN_TEST_FS_HELPER"
	testBlockEnv       = "AIHARN_TEST_FS_HELPER_BLOCK_REPLACE"
	testBlockSignalEnv = "AIHARN_TEST_FS_HELPER_SIGNAL"
	testHelperArg      = "--test-fs-helper"
)

// TestMain doubles as the filesystem helper. The SSH transport starts this test
// binary as the remote helper command, and the harness passes the test process's
// environment through, so a re-executed binary sees the guard variable and
// serves fsproto on its stdin/stdout before the testing framework runs.
func TestMain(m *testing.M) {
	if os.Getenv(testFSHelperEnv) == "1" {
		os.Exit(testFSHelperMain())
	}
	if err := os.Setenv(testFSHelperEnv, "1"); err != nil {
		fmt.Fprintf(os.Stderr, "set %s: %v\n", testFSHelperEnv, err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

// testFSHelperMain serves one helper session on the real fscore backend.
func testFSHelperMain() int {
	if err := fshelper.Serve(os.Stdin, os.Stdout, openTestFSHelperBackend); err != nil {
		fmt.Fprintf(os.Stderr, "test fs-helper: %v\n", err)
		return 1
	}
	return 0
}

// openTestFSHelperBackend opens the confined backend for root, optionally
// wrapped so a test can hold a mutation open until the connection dies.
func openTestFSHelperBackend(root string) (fshelper.Backend, error) {
	service, err := fscore.Open(root)
	if err != nil {
		return nil, err
	}
	if os.Getenv(testBlockEnv) == "1" {
		return &blockingReplaceBackend{Backend: service, signalPath: os.Getenv(testBlockSignalEnv)}, nil
	}
	return service, nil
}

// blockingReplaceBackend announces that a replace reached the backend and then
// never answers, so the client's own failure handling (disconnect or context
// cancellation) decides the outcome.
type blockingReplaceBackend struct {
	fshelper.Backend
	signalPath string
}

func (b *blockingReplaceBackend) Replace(string, []byte, execution.FileVersion) (execution.FileStat, error) {
	if b.signalPath != "" {
		_ = os.WriteFile(b.signalPath, []byte("replace started\n"), 0o600)
	}
	select {}
}

// testHelperCommand returns the remote command that starts this test binary as
// the helper. The harness runs it through a shell, so the path is quoted.
func testHelperCommand(t *testing.T) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("resolve test executable: %v", err)
	}
	return shellQuote(exe) + " " + testHelperArg
}

// shellQuote single-quotes s for POSIX shells.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// startHarnessFS starts an in-process SSH server and opens a filesystem for root
// through the real transport, using this test binary as the helper.
func startHarnessFS(t *testing.T, root string) (execution.Filesystem, *harness.Server) {
	t.Helper()
	srv := newServer(t)
	tr := newTransport(t, srv, execssh.Options{Insecure: true, FSHelper: testHelperCommand(t)})
	fs, err := tr.NewFilesystem(context.Background(), root)
	if err != nil {
		t.Fatalf("NewFilesystem(%q): %v", root, err)
	}
	t.Cleanup(func() { _ = fs.Close() })
	return fs, srv
}

// waitForSignal blocks until path exists, so a test can act only once the
// helper has really started serving a request.
func waitForSignal(t *testing.T, path string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("helper did not signal %s within %v", path, timeout)
}

// TestFilesystemContract runs the frozen filesystem contract over the SSH
// transport and the framed helper protocol.
func TestFilesystemContract(t *testing.T) {
	fstest.Contract(t, func(t *testing.T) (execution.Filesystem, string) {
		root := t.TempDir()
		fs, _ := startHarnessFS(t, root)
		return fs, root
	})
}

func TestFilesystemTargetNamesUserHostPortAndRoot(t *testing.T) {
	root := t.TempDir()
	fs, srv := startHarnessFS(t, root)
	want := fmt.Sprintf("ssh:test@127.0.0.1:%d|%s", srv.Port(), root)
	if got := fs.Target(); got != want {
		t.Fatalf("Target() = %q, want %q", got, want)
	}
}

func TestNewFilesystemRejectsRelativeRoot(t *testing.T) {
	srv := newServer(t)
	tr := newTransport(t, srv, execssh.Options{Insecure: true})
	for _, root := range []string{"", "relative/path", "/tmp/../etc"} {
		_, err := tr.NewFilesystem(context.Background(), root)
		if code := execution.FSECode(err); code != execution.FSOutsideRoot {
			t.Fatalf("NewFilesystem(%q) code = %q, want %q (%v)", root, code, execution.FSOutsideRoot, err)
		}
	}
}

func TestNewFilesystemMissingHelperIsUnsupported(t *testing.T) {
	srv := newServer(t)
	tr := newTransport(t, srv, execssh.Options{Insecure: true, FSHelper: "aiharn-fs-helper-does-not-exist-7f3a"})
	// The helper exits immediately (command not found). The handshake must fail
	// as soon as the process exits instead of waiting for the init timeout.
	start := time.Now()
	_, err := tr.NewFilesystem(context.Background(), t.TempDir())
	if code := execution.FSECode(err); code != execution.FSUnsupportedFilesystem {
		t.Fatalf("code = %q, want %q (%v)", code, execution.FSUnsupportedFilesystem, err)
	}
	for _, want := range []string{"fs_helper", "fs-helper", "install"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error does not explain how to fix the helper (%q missing): %v", want, err)
		}
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("missing helper took %v, want a prompt failure", elapsed)
	}
}

func TestNewFilesystemBogusHelperFrameFailsFast(t *testing.T) {
	// A helper that announces a 5-byte frame and sends non-JSON: the client must
	// reject the frame without waiting for more input or allocating an announced
	// size.
	srv := newServer(t)
	tr := newTransport(t, srv, execssh.Options{Insecure: true, FSHelper: `printf '\000\000\000\005hello'`})
	start := time.Now()
	_, err := tr.NewFilesystem(context.Background(), t.TempDir())
	if code := execution.FSECode(err); code != execution.FSUnsupportedFilesystem {
		t.Fatalf("code = %q, want %q (%v)", code, execution.FSUnsupportedFilesystem, err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("bogus helper frame took %v, want a prompt failure", elapsed)
	}
}

func TestNewFilesystemHelperProtocolMismatchIsUnsupported(t *testing.T) {
	// A helper that answers init with a well-formed frame for another protocol
	// version must be refused with install guidance, not silently accepted.
	srv := newServer(t)
	tr := newTransport(t, srv, execssh.Options{Insecure: true, FSHelper: `printf '\000\000\000\027{"ok":true,"protocol":2}'`})
	_, err := tr.NewFilesystem(context.Background(), t.TempDir())
	if code := execution.FSECode(err); code != execution.FSUnsupportedFilesystem {
		t.Fatalf("code = %q, want %q (%v)", code, execution.FSUnsupportedFilesystem, err)
	}
	if !strings.Contains(err.Error(), "protocol") {
		t.Fatalf("error does not mention the protocol mismatch: %v", err)
	}
}

func TestNewFilesystemOversizedHelperFrameFailsFast(t *testing.T) {
	// The frame header announces 1 GiB. ReadFrame rejects it from the header, so
	// the client must not attempt to read or allocate the body.
	srv := newServer(t)
	tr := newTransport(t, srv, execssh.Options{Insecure: true, FSHelper: `printf '\077\377\377\377'`})
	start := time.Now()
	_, err := tr.NewFilesystem(context.Background(), t.TempDir())
	if code := execution.FSECode(err); code != execution.FSUnsupportedFilesystem {
		t.Fatalf("code = %q, want %q (%v)", code, execution.FSUnsupportedFilesystem, err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("oversized helper frame took %v, want a prompt failure", elapsed)
	}
}

// blockReplaceOnHelper configures the re-executed helper to hold every replace
// open and to announce it through the returned signal path.
func blockReplaceOnHelper(t *testing.T) string {
	t.Helper()
	signalPath := filepath.Join(t.TempDir(), "replace-started")
	t.Setenv(testBlockEnv, "1")
	t.Setenv(testBlockSignalEnv, signalPath)
	return signalPath
}

func TestReplaceDuringDisconnectIsOutcomeUnknown(t *testing.T) {
	signalPath := blockReplaceOnHelper(t)
	root := t.TempDir()
	fs, srv := startHarnessFS(t, root)
	created, err := fs.CreateFile(context.Background(), "a.txt", []byte("one\n"))
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	result := make(chan error, 1)
	go func() {
		_, err := fs.ReplaceFile(context.Background(), "a.txt", []byte("two\n"), created.Version)
		result <- err
	}()
	waitForSignal(t, signalPath, 10*time.Second)
	if err := srv.Close(); err != nil {
		t.Fatalf("close harness: %v", err)
	}

	select {
	case err := <-result:
		if code := execution.FSECode(err); code != execution.FSOutcomeUnknown {
			t.Fatalf("code = %q, want %q (%v)", code, execution.FSOutcomeUnknown, err)
		}
		if !strings.Contains(err.Error(), "read the file again") {
			t.Fatalf("error does not tell the caller to re-read: %v", err)
		}
		if strings.Contains(strings.ToLower(err.Error()), "rolled back") {
			t.Fatalf("error claims a rollback: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("replace did not fail after the connection dropped")
	}
}

func TestReplaceWithCanceledContextIsOutcomeUnknown(t *testing.T) {
	signalPath := blockReplaceOnHelper(t)
	root := t.TempDir()
	fs, _ := startHarnessFS(t, root)
	created, err := fs.CreateFile(context.Background(), "a.txt", []byte("one\n"))
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := fs.ReplaceFile(ctx, "a.txt", []byte("two\n"), created.Version)
		result <- err
	}()
	waitForSignal(t, signalPath, 10*time.Second)
	cancel()

	select {
	case err := <-result:
		if code := execution.FSECode(err); code != execution.FSOutcomeUnknown {
			t.Fatalf("code = %q, want %q (%v)", code, execution.FSOutcomeUnknown, err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("canceled replace did not return")
	}
	// The canceled request dropped the connection, so later reads fail fast
	// instead of hanging on a desynchronized stream.
	start := time.Now()
	if _, err := fs.ReadFile(context.Background(), "a.txt", 16); execution.FSECode(err) != execution.FSIOError {
		t.Fatalf("read after cancellation code = %q, want %q (%v)", execution.FSECode(err), execution.FSIOError, err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("read after cancellation took %v", elapsed)
	}
}

func TestReadWithCanceledContextBeforeRequestIsIOError(t *testing.T) {
	root := t.TempDir()
	fs, _ := startHarnessFS(t, root)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := fs.ReadFile(ctx, "a.txt", 16)
	if code := execution.FSECode(err); code != execution.FSIOError {
		t.Fatalf("code = %q, want %q (%v)", code, execution.FSIOError, err)
	}
}

func TestFilesystemCloseIsIdempotentAndFailsLaterRequests(t *testing.T) {
	root := t.TempDir()
	fs, _ := startHarnessFS(t, root)
	if err := fs.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := fs.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if _, err := fs.ReadFile(context.Background(), "a.txt", 16); execution.FSECode(err) != execution.FSIOError {
		t.Fatalf("read after close code = %q, want %q (%v)", execution.FSECode(err), execution.FSIOError, err)
	}
	if _, err := fs.CreateFile(context.Background(), "a.txt", []byte("x")); execution.FSECode(err) != execution.FSIOError {
		t.Fatalf("create after close code = %q, want %q (%v)", execution.FSECode(err), execution.FSIOError, err)
	}
}

func TestFilesystemDoesNotDisturbSessionClient(t *testing.T) {
	srv := newServer(t)
	tr := newTransport(t, srv, execssh.Options{Insecure: true, KeepAlive: true, FSHelper: testHelperCommand(t)})
	session := openSession(t, tr)
	if r := execCmd(t, session, "printf before"); r.Stdout != "before" {
		t.Fatalf("stdout = %q, want before", r.Stdout)
	}
	root := t.TempDir()
	fs, err := tr.NewFilesystem(context.Background(), root)
	if err != nil {
		t.Fatalf("NewFilesystem: %v", err)
	}
	if _, err := fs.CreateFile(context.Background(), "a.txt", []byte("x")); err != nil {
		t.Fatalf("create through filesystem: %v", err)
	}
	if err := fs.Close(); err != nil {
		t.Fatalf("filesystem close: %v", err)
	}
	// The session's own shell must survive both the filesystem's dedicated
	// connection and its Close.
	if r := execCmd(t, session, "printf after"); r.Stdout != "after" {
		t.Fatalf("stdout after filesystem close = %q, want after", r.Stdout)
	}
}
