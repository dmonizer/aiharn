// Package fstest supplies a reusable contract suite for execution.Filesystem
// implementations. Local and remote backends must both pass it unchanged.
package fstest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"aiharn/internal/execution"
)

// Factory returns a backend rooted at a fresh temporary directory together with
// that directory's absolute path. Each call creates an independent root.
type Factory func(t *testing.T) (fs execution.Filesystem, root string)

// Contract runs the filesystem contract against the backend produced by newFS.
// They are ordinary filesystem semantics available on POSIX hosts.
func Contract(t *testing.T, newFS Factory) {
	t.Run("create_read_roundtrip", func(t *testing.T) { testCreateRead(t, newFS) })
	t.Run("create_empty", func(t *testing.T) { testCreateEmpty(t, newFS) })
	t.Run("create_missing_parent", func(t *testing.T) { testCreateMissingParent(t, newFS) })
	t.Run("create_existing", func(t *testing.T) { testCreateExisting(t, newFS) })
	t.Run("create_dangling_symlink", func(t *testing.T) { testCreateDanglingSymlink(t, newFS) })
	t.Run("read_missing", func(t *testing.T) { testReadMissing(t, newFS) })
	t.Run("read_directory", func(t *testing.T) { testReadDirectory(t, newFS) })
	t.Run("read_symlink_leaf", func(t *testing.T) { testReadSymlinkLeaf(t, newFS) })
	t.Run("read_symlink_parent", func(t *testing.T) { testReadSymlinkParent(t, newFS) })
	t.Run("path_outside_root", func(t *testing.T) { testOutsideRoot(t, newFS) })
	t.Run("read_byte_limit", func(t *testing.T) { testReadByteLimit(t, newFS) })
	t.Run("replace", func(t *testing.T) { testReplace(t, newFS) })
	t.Run("replace_stale", func(t *testing.T) { testReplaceStale(t, newFS) })
	t.Run("replace_missing", func(t *testing.T) { testReplaceMissing(t, newFS) })
	t.Run("replace_preserves_mode", func(t *testing.T) { testReplaceMode(t, newFS) })
	t.Run("replace_hard_link", func(t *testing.T) { testReplaceHardLink(t, newFS) })
	t.Run("target", func(t *testing.T) { testTarget(t, newFS) })
	t.Run("shell_metacharacters", func(t *testing.T) { testShellMetacharacters(t, newFS) })
	t.Run("sibling_prefix_escape", func(t *testing.T) { testSiblingPrefix(t, newFS) })
}

func testCreateRead(t *testing.T, newFS Factory) {
	fs, _ := newFS(t)
	t.Cleanup(func() { _ = fs.Close() })
	const content = "hello\nworld\n"
	stat, err := fs.CreateFile(context.Background(), "a.txt", []byte(content))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if stat.Name != "a.txt" {
		t.Fatalf("canonical name = %q, want a.txt", stat.Name)
	}
	if stat.Version.Size != int64(len(content)) {
		t.Fatalf("size = %d, want %d", stat.Version.Size, len(content))
	}
	if stat.Version.Digest != digest(content) {
		t.Fatalf("digest = %q, want %q", stat.Version.Digest, digest(content))
	}
	if stat.LinkCount != 1 {
		t.Fatalf("link count = %d, want 1", stat.LinkCount)
	}
	snap, err := fs.ReadFile(context.Background(), "a.txt", 1<<20)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(snap.Content) != content {
		t.Fatalf("content = %q, want %q", snap.Content, content)
	}
	if !snap.Stat.Version.Equal(stat.Version) {
		t.Fatalf("read version %+v != create version %+v", snap.Stat.Version, stat.Version)
	}
}

func testCreateEmpty(t *testing.T, newFS Factory) {
	fs, _ := newFS(t)
	t.Cleanup(func() { _ = fs.Close() })
	if _, err := fs.CreateFile(context.Background(), "empty.txt", nil); err != nil {
		t.Fatalf("create empty: %v", err)
	}
	snap, err := fs.ReadFile(context.Background(), "empty.txt", 16)
	if err != nil {
		t.Fatalf("read empty: %v", err)
	}
	if len(snap.Content) != 0 {
		t.Fatalf("content = %q, want empty", snap.Content)
	}
}

func testCreateMissingParent(t *testing.T, newFS Factory) {
	fs, _ := newFS(t)
	t.Cleanup(func() { _ = fs.Close() })
	_, err := fs.CreateFile(context.Background(), "missing/a.txt", []byte("x"))
	requireCode(t, err, execution.FSParentNotFound)
}

func testCreateExisting(t *testing.T, newFS Factory) {
	fs, _ := newFS(t)
	t.Cleanup(func() { _ = fs.Close() })
	ctx := context.Background()
	if _, err := fs.CreateFile(ctx, "a.txt", []byte("one")); err != nil {
		t.Fatalf("first create: %v", err)
	}
	_, err := fs.CreateFile(ctx, "a.txt", []byte("two"))
	requireCode(t, err, execution.FSAlreadyExists)
	snap, err := fs.ReadFile(ctx, "a.txt", 16)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(snap.Content) != "one" {
		t.Fatalf("existing file was overwritten: %q", snap.Content)
	}
}

func testCreateDanglingSymlink(t *testing.T, newFS Factory) {
	fs, root := newFS(t)
	t.Cleanup(func() { _ = fs.Close() })
	if err := os.Symlink(filepath.Join(root, "nowhere"), filepath.Join(root, "dangling")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	_, err := fs.CreateFile(context.Background(), "dangling", []byte("x"))
	requireCode(t, err, execution.FSAlreadyExists)
}

func testReadMissing(t *testing.T, newFS Factory) {
	fs, _ := newFS(t)
	t.Cleanup(func() { _ = fs.Close() })
	_, err := fs.ReadFile(context.Background(), "nope.txt", 16)
	requireCode(t, err, execution.FSNotFound)
}

func testReadDirectory(t *testing.T, newFS Factory) {
	fs, root := newFS(t)
	t.Cleanup(func() { _ = fs.Close() })
	if err := os.Mkdir(filepath.Join(root, "dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := fs.ReadFile(context.Background(), "dir", 16)
	requireCode(t, err, execution.FSNotRegularFile)
	_, err = fs.ReadFile(context.Background(), ".", 16)
	requireCode(t, err, execution.FSNotRegularFile)
}

func testReadSymlinkLeaf(t *testing.T, newFS Factory) {
	fs, root := newFS(t)
	t.Cleanup(func() { _ = fs.Close() })
	ctx := context.Background()
	if _, err := fs.CreateFile(ctx, "real.txt", []byte("x")); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := os.Symlink(filepath.Join(root, "real.txt"), filepath.Join(root, "link.txt")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	_, err := fs.ReadFile(ctx, "link.txt", 16)
	requireCode(t, err, execution.FSSymlinkNotAllowed)
}

func testReadSymlinkParent(t *testing.T, newFS Factory) {
	fs, root := newFS(t)
	t.Cleanup(func() { _ = fs.Close() })
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	_, err := fs.ReadFile(context.Background(), "escape/secret.txt", 1024)
	requireCode(t, err, execution.FSSymlinkNotAllowed)
}

func testOutsideRoot(t *testing.T, newFS Factory) {
	fs, root := newFS(t)
	t.Cleanup(func() { _ = fs.Close() })
	ctx := context.Background()
	for _, name := range []string{"", "..", "../a", "a/../../b", "/etc/passwd", filepath.Join(root, "..", "x")} {
		if _, err := fs.ReadFile(ctx, name, 16); err == nil {
			t.Fatalf("read %q succeeded, want outside_root", name)
		} else if code := execution.FSECode(err); code != execution.FSOutsideRoot {
			t.Fatalf("read %q code = %q, want %q (%v)", name, code, execution.FSOutsideRoot, err)
		}
	}
	// "." components are harmless and canonicalized away; ".." is not.
	if _, err := fs.CreateFile(ctx, "./dot.txt", []byte("x")); err != nil {
		t.Fatalf("create with a dot component: %v", err)
	}
	if snap, err := fs.ReadFile(ctx, "./dot.txt", 16); err != nil || snap.Stat.Name != "dot.txt" {
		t.Fatalf("dot component read: %+v %v", snap.Stat, err)
	}
	// The root itself is a directory, not a regular file.
	requireCode(t, func() error {
		_, err := fs.ReadFile(ctx, ".", 16)
		return err
	}(), execution.FSNotRegularFile)

	// An absolute path inside the root is accepted and canonicalized.
	if _, err := fs.CreateFile(ctx, filepath.Join(root, "abs.txt"), []byte("x")); err != nil {
		t.Fatalf("create absolute inside root: %v", err)
	}
	snap, err := fs.ReadFile(ctx, filepath.Join(root, "abs.txt"), 16)
	if err != nil {
		t.Fatalf("read absolute inside root: %v", err)
	}
	if snap.Stat.Name != "abs.txt" {
		t.Fatalf("canonical name = %q, want abs.txt", snap.Stat.Name)
	}
}

func testReadByteLimit(t *testing.T, newFS Factory) {
	fs, _ := newFS(t)
	t.Cleanup(func() { _ = fs.Close() })
	ctx := context.Background()
	if _, err := fs.CreateFile(ctx, "big.txt", []byte("0123456789")); err != nil {
		t.Fatal(err)
	}
	_, err := fs.ReadFile(ctx, "big.txt", 9)
	requireCode(t, err, execution.FSFileLimitExceeded)
	if _, err := fs.ReadFile(ctx, "big.txt", 10); err != nil {
		t.Fatalf("read at exact limit: %v", err)
	}
}

func testReplace(t *testing.T, newFS Factory) {
	fs, _ := newFS(t)
	t.Cleanup(func() { _ = fs.Close() })
	ctx := context.Background()
	created, err := fs.CreateFile(ctx, "a.txt", []byte("one\n"))
	if err != nil {
		t.Fatal(err)
	}
	updated, err := fs.ReplaceFile(ctx, "a.txt", []byte("two\nlonger\n"), created.Version)
	if err != nil {
		t.Fatalf("replace: %v", err)
	}
	if updated.Version.Digest != digest("two\nlonger\n") {
		t.Fatalf("digest = %q", updated.Version.Digest)
	}
	snap, err := fs.ReadFile(ctx, "a.txt", 64)
	if err != nil {
		t.Fatal(err)
	}
	if string(snap.Content) != "two\nlonger\n" {
		t.Fatalf("content = %q", snap.Content)
	}
}

func testReplaceStale(t *testing.T, newFS Factory) {
	fs, _ := newFS(t)
	t.Cleanup(func() { _ = fs.Close() })
	ctx := context.Background()
	created, err := fs.CreateFile(ctx, "a.txt", []byte("one\n"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fs.ReplaceFile(ctx, "a.txt", []byte("two\n"), created.Version); err != nil {
		t.Fatal(err)
	}
	_, err = fs.ReplaceFile(ctx, "a.txt", []byte("three\n"), created.Version)
	requireCode(t, err, execution.FSStaleRead)
	snap, err := fs.ReadFile(ctx, "a.txt", 64)
	if err != nil {
		t.Fatal(err)
	}
	if string(snap.Content) != "two\n" {
		t.Fatalf("stale replace changed content: %q", snap.Content)
	}
}

func testReplaceMissing(t *testing.T, newFS Factory) {
	fs, _ := newFS(t)
	t.Cleanup(func() { _ = fs.Close() })
	_, err := fs.ReplaceFile(context.Background(), "nope.txt", []byte("x"), execution.FileVersion{})
	requireCode(t, err, execution.FSNotFound)
}

func testReplaceMode(t *testing.T, newFS Factory) {
	if os.Geteuid() == 0 {
		t.Skip("permission bits are not enforced for root")
	}
	fs, root := newFS(t)
	t.Cleanup(func() { _ = fs.Close() })
	ctx := context.Background()
	created, err := fs.CreateFile(ctx, "a.txt", []byte("one\n"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(root, "a.txt"), 0o640); err != nil {
		t.Fatal(err)
	}
	snap, err := fs.ReadFile(ctx, "a.txt", 64)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fs.ReplaceFile(ctx, "a.txt", []byte("two\n"), snap.Stat.Version); err != nil {
		t.Fatalf("replace: %v", err)
	}
	info, err := os.Stat(filepath.Join(root, "a.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o640 {
		t.Fatalf("mode = %o, want 640", info.Mode().Perm())
	}
	_ = created
}

func testReplaceHardLink(t *testing.T, newFS Factory) {
	fs, root := newFS(t)
	t.Cleanup(func() { _ = fs.Close() })
	ctx := context.Background()
	created, err := fs.CreateFile(ctx, "a.txt", []byte("one\n"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(root, "a.txt"), filepath.Join(root, "b.txt")); err != nil {
		t.Skipf("hard link unsupported: %v", err)
	}
	_, err = fs.ReplaceFile(ctx, "a.txt", []byte("two\n"), created.Version)
	requireCode(t, err, execution.FSHardLinksNotAllowed)
}

func testTarget(t *testing.T, newFS Factory) {
	fs, _ := newFS(t)
	t.Cleanup(func() { _ = fs.Close() })
	if fs.Target() == "" {
		t.Fatal("Target() is empty")
	}
}

// testShellMetacharacters proves names and content are treated as literal data:
// nothing is shell-expanded, and no side effect appears on the host.
func testShellMetacharacters(t *testing.T, newFS Factory) {
	fs, root := newFS(t)
	t.Cleanup(func() { _ = fs.Close() })
	ctx := context.Background()
	const name = `weird ; touch pwned $(id) 'quoted' "double".txt`
	content := "echo $(touch pwned) `id` ; rm -rf /  \n & | > < *\n"
	if _, err := fs.CreateFile(ctx, name, []byte(content)); err != nil {
		t.Fatalf("create with metacharacters: %v", err)
	}
	snap, err := fs.ReadFile(ctx, name, 1<<20)
	if err != nil {
		t.Fatalf("read with metacharacters: %v", err)
	}
	if string(snap.Content) != content {
		t.Fatalf("content = %q, want %q", snap.Content, content)
	}
	if _, err := os.Stat(filepath.Join(root, "pwned")); !os.IsNotExist(err) {
		t.Fatalf("a metacharacter side effect appeared: %v", err)
	}
	if _, err := fs.ReplaceFile(ctx, name, []byte("still ; literal\n"), snap.Stat.Version); err != nil {
		t.Fatalf("replace with metacharacters: %v", err)
	}
}

// testSiblingPrefix proves containment is component-aware: a directory whose
// name shares the root's prefix is outside the root.
func testSiblingPrefix(t *testing.T, newFS Factory) {
	fs, root := newFS(t)
	t.Cleanup(func() { _ = fs.Close() })
	sibling := root + "-sibling"
	if err := os.MkdirAll(sibling, 0o755); err != nil {
		t.Skipf("cannot create a sibling directory: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sibling) })
	if err := os.WriteFile(filepath.Join(sibling, "secret.txt"), []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := fs.ReadFile(context.Background(), filepath.Join(sibling, "secret.txt"), 1024)
	requireCode(t, err, execution.FSOutsideRoot)
}

func requireCode(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error with code %q, got nil", want)
	}
	if code := execution.FSECode(err); code != want {
		t.Fatalf("code = %q, want %q (err: %v)", code, want, err)
	}
}

func digest(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}
