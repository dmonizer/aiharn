package fscore_test

import (
	"crypto/sha256"
	"encoding/hex"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"

	"aiharn/internal/execution"
	"aiharn/internal/execution/fscore"
)

// openService opens a confined service on a fresh temporary root.
func openService(t *testing.T) (*fscore.Service, string) {
	t.Helper()
	root := t.TempDir()
	service, err := fscore.Open(root)
	if err != nil {
		t.Fatalf("Open(%q): %v", root, err)
	}
	t.Cleanup(func() { _ = service.Close() })
	return service, root
}

// requireCode fails unless err is an *execution.FSError with the wanted code.
func requireCode(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error with code %q, got nil", want)
	}
	if code := execution.FSECode(err); code != want {
		t.Fatalf("code = %q, want %q (err: %v)", code, want, err)
	}
}

// digestOf returns the lowercase hex SHA-256 of content.
func digestOf(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

func TestOpenRejectsInvalidRoot(t *testing.T) {
	base := t.TempDir()
	insideDir := filepath.Join(base, "dir")
	if err := os.Mkdir(insideDir, 0o755); err != nil {
		t.Fatal(err)
	}
	regularFile := filepath.Join(base, "file")
	if err := os.WriteFile(regularFile, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(insideDir, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}

	cases := map[string]struct {
		root string
		want string
	}{
		"relative":     {root: "relative/root", want: execution.FSOutsideRoot},
		"empty":        {root: "", want: execution.FSOutsideRoot},
		"missing":      {root: filepath.Join(base, "missing"), want: execution.FSNotFound},
		"regular file": {root: regularFile, want: execution.FSNotRegularFile},
		"symlink":      {root: link, want: execution.FSSymlinkNotAllowed},
	}
	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			service, err := fscore.Open(testCase.root)
			if err == nil {
				_ = service.Close()
				t.Fatalf("Open(%q) succeeded, want an error", testCase.root)
			}
			requireCode(t, err, testCase.want)
		})
	}
}

func TestTargetIsCanonicalRoot(t *testing.T) {
	service, root := openService(t)
	if got, want := service.Target(), filepath.Clean(root); got != want {
		t.Fatalf("Target() = %q, want %q", got, want)
	}
}

func TestReadRejectsNonRegularLeaf(t *testing.T) {
	createFIFO := func(t *testing.T, path string) {
		t.Helper()
		if err := unix.Mkfifo(path, 0o600); err != nil {
			t.Skipf("mkfifo unsupported: %v", err)
		}
	}
	createSocket := func(t *testing.T, path string) {
		t.Helper()
		listener, err := net.Listen("unix", path)
		if err != nil {
			t.Skipf("unix socket unsupported: %v", err)
		}
		t.Cleanup(func() { _ = listener.Close() })
	}
	createDirectory := func(t *testing.T, path string) {
		t.Helper()
		if err := os.Mkdir(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	cases := map[string]func(t *testing.T, path string){
		"fifo":      createFIFO,
		"socket":    createSocket,
		"directory": createDirectory,
	}
	for name, create := range cases {
		t.Run(name, func(t *testing.T) {
			service, root := openService(t)
			create(t, filepath.Join(root, "entry"))

			_, err := service.Read("entry", 1024)
			requireCode(t, err, execution.FSNotRegularFile)

			_, err = service.Replace("entry", []byte("x"), execution.FileVersion{})
			requireCode(t, err, execution.FSNotRegularFile)

			_, err = service.Create("entry", []byte("x"))
			requireCode(t, err, execution.FSAlreadyExists)
		})
	}
}

func TestReadRejectsDeepSymlinkComponent(t *testing.T) {
	service, root := openService(t)
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(root, "one", "two")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(nested, "escape")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}

	_, err := service.Read("one/two/escape/secret.txt", 1024)
	requireCode(t, err, execution.FSSymlinkNotAllowed)
}

func TestReadRejectsEscapingNames(t *testing.T) {
	service, root := openService(t)
	outside := t.TempDir()
	names := []string{
		"",
		"../outside.txt",
		"dir/../../outside.txt",
		filepath.Join(outside, "outside.txt"),
		filepath.Join(root, "..", "outside.txt"),
		"/etc/passwd",
	}
	for _, name := range names {
		if _, err := service.Read(name, 16); !execution.IsFSECode(err, execution.FSOutsideRoot) {
			t.Fatalf("Read(%q) = %v, want %q", name, err, execution.FSOutsideRoot)
		}
	}
}

func TestReadAcceptsAbsoluteNameInsideRoot(t *testing.T) {
	service, root := openService(t)
	created, err := service.Create(filepath.Join(root, "abs.txt"), []byte("data"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if created.Name != "abs.txt" {
		t.Fatalf("canonical name = %q, want abs.txt", created.Name)
	}
	snapshot, err := service.Read(filepath.Join(root, "abs.txt"), 16)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if snapshot.Stat.Name != "abs.txt" || string(snapshot.Content) != "data" {
		t.Fatalf("snapshot = %+v content %q", snapshot.Stat, snapshot.Content)
	}
}

func TestReadRootItselfIsNotARegularFile(t *testing.T) {
	service, root := openService(t)
	for _, name := range []string{".", root, root + "/", root + "/."} {
		_, err := service.Read(name, 16)
		if !execution.IsFSECode(err, execution.FSNotRegularFile) {
			t.Fatalf("Read(%q) = %v, want %q", name, err, execution.FSNotRegularFile)
		}
	}
}

func TestCreateRejectsEscapingNames(t *testing.T) {
	service, root := openService(t)
	for _, name := range []string{"", "../new.txt", "dir/../../new.txt", filepath.Join(root, "..", "new.txt")} {
		_, err := service.Create(name, []byte("x"))
		if !execution.IsFSECode(err, execution.FSOutsideRoot) {
			t.Fatalf("Create(%q) = %v, want %q", name, err, execution.FSOutsideRoot)
		}
	}
}

func TestReadCanonicalizesHarmlessComponents(t *testing.T) {
	service, root := openService(t)
	if err := os.Mkdir(filepath.Join(root, "dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Create("dir/file.txt", []byte("data")); err != nil {
		t.Fatalf("Create: %v", err)
	}
	for _, name := range []string{
		"dir/file.txt",
		"dir//file.txt",
		"dir/.//file.txt",
		"dir/file.txt/",
		filepath.Join(root, "dir", "file.txt"),
	} {
		snapshot, err := service.Read(name, 16)
		if err != nil {
			t.Fatalf("Read(%q): %v", name, err)
		}
		if snapshot.Stat.Name != "dir/file.txt" {
			t.Fatalf("Read(%q) canonical name = %q, want dir/file.txt", name, snapshot.Stat.Name)
		}
	}
}

func TestReadDigestMatchesContent(t *testing.T) {
	cases := map[string]string{
		"empty":        "",
		"multibyte":    "héllo, 世界 — ünïcode\n",
		"binary":       "\x00\x01\x02\xff\xfe",
		"multiline":    "one\ntwo\nthree\n",
		"whitespace":   "  \t\n",
		"large latin1": strings.Repeat("abcdefgh", 1024),
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			service, _ := openService(t)
			created, err := service.Create("file.bin", []byte(content))
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			want := digestOf(content)
			if created.Version.Digest != want {
				t.Fatalf("create digest = %q, want %q", created.Version.Digest, want)
			}
			if created.Version.Size != int64(len(content)) {
				t.Fatalf("create size = %d, want %d", created.Version.Size, len(content))
			}
			snapshot, err := service.Read("file.bin", 1<<20)
			if err != nil {
				t.Fatalf("Read: %v", err)
			}
			if snapshot.Stat.Version.Digest != want {
				t.Fatalf("read digest = %q, want %q", snapshot.Stat.Version.Digest, want)
			}
			if string(snapshot.Content) != content {
				t.Fatalf("content = %q, want %q", snapshot.Content, content)
			}
			if !snapshot.Stat.Version.Equal(created.Version) {
				t.Fatalf("read version %+v != create version %+v", snapshot.Stat.Version, created.Version)
			}
		})
	}
}

func TestReadPermissionDeniedParent(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission bits are not enforced for root")
	}
	service, root := openService(t)
	blocked := filepath.Join(root, "blocked")
	if err := os.Mkdir(blocked, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(blocked, "target.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(blocked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(blocked, 0o755) })

	_, err := service.Read("blocked/target.txt", 16)
	requireCode(t, err, execution.FSPermissionDenied)
	_, err = service.Create("blocked/new.txt", []byte("x"))
	requireCode(t, err, execution.FSPermissionDenied)
}

func TestFailedMutationsLeaveNoStagingFiles(t *testing.T) {
	service, root := openService(t)
	created, err := service.Create("a.txt", []byte("one\n"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := service.Replace("a.txt", []byte("two\n"), created.Version); err != nil {
		t.Fatalf("Replace: %v", err)
	}
	// A stale version must not publish and must not leave a staging file behind.
	_, err = service.Replace("a.txt", []byte("three\n"), created.Version)
	requireCode(t, err, execution.FSStaleRead)
	// A rejected create must not leave one either.
	_, err = service.Create("a.txt", []byte("four\n"))
	requireCode(t, err, execution.FSAlreadyExists)

	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".aiharn-tmp-") {
			t.Fatalf("staging file %q was left behind", entry.Name())
		}
	}
	if len(entries) != 1 || entries[0].Name() != "a.txt" {
		t.Fatalf("root entries = %v, want just a.txt", entries)
	}
}

func TestReplacePreservesOwnershipAndMode(t *testing.T) {
	service, root := openService(t)
	if _, err := service.Create("script.sh", []byte("run\n")); err != nil {
		t.Fatalf("Create: %v", err)
	}
	path := filepath.Join(root, "script.sh")
	if err := os.Chmod(path, 0o751); err != nil {
		t.Fatal(err)
	}
	before, err := service.Read("script.sh", 64)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	after, err := service.Replace("script.sh", []byte("run again\n"), before.Stat.Version)
	if err != nil {
		t.Fatalf("Replace: %v", err)
	}
	if after.Mode != 0o751 {
		t.Fatalf("mode = %o, want 751", after.Mode)
	}
	if after.UID != before.Stat.UID || after.GID != before.Stat.GID {
		t.Fatalf("ownership = %d:%d, want %d:%d", after.UID, after.GID, before.Stat.UID, before.Stat.GID)
	}
	if after.Version.Digest != digestOf("run again\n") {
		t.Fatalf("digest = %q", after.Version.Digest)
	}
	if after.LinkCount != 1 {
		t.Fatalf("link count = %d, want 1", after.LinkCount)
	}
}

func TestReadAfterCloseFails(t *testing.T) {
	service, _ := openService(t)
	if err := service.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := service.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	_, err := service.Read("a.txt", 16)
	requireCode(t, err, execution.FSIOError)
}
