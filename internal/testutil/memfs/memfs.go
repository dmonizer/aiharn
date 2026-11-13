// Package memfs provides an in-memory execution.Filesystem test double. It is
// deliberately permissive about paths (confinement is the real backends' job)
// and gives tests precise control over versions, failures, and external
// modifications made between a proposal and its commit.
package memfs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path"
	"sort"
	"strings"
	"sync"

	"aiharn/internal/execution"
)

// File is one in-memory regular file.
type File struct {
	Content   string
	Version   execution.FileVersion
	Mode      uint32
	LinkCount uint64
}

// FS is an in-memory execution.Filesystem.
type FS struct {
	// TargetName is returned by Target.
	TargetName string
	// MaxBytes mimics the backend limit; zero means unlimited.
	MaxBytes int64

	mu      sync.Mutex
	files   map[string]*File
	nextID  uint64
	failure string // pending injected failure code
	calls   []string
	closed  bool
	setHook func(name string, content string)
}

// New returns an empty in-memory filesystem.
func New() *FS {
	return &FS{TargetName: "memfs:/root", files: map[string]*File{}}
}

var _ execution.Filesystem = (*FS)(nil)

// Set installs content at name, as an external writer would. It bypasses the
// tool ledger so tests can simulate untracked modifications.
func (f *FS) Set(name, content string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.put(name, content)
}

// Rewrite changes content while keeping the same logical lines, simulating an
// external rewrite with a different version (for example identical bytes
// rewritten by another process: use Touch for that case).
func (f *FS) Rewrite(name, content string) { f.Set(name, content) }

// Touch bumps the version of name without changing its content, simulating an
// identical-byte external rewrite.
func (f *FS) Touch(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	file := f.files[canonical(name)]
	if file == nil {
		return
	}
	f.nextID++
	file.Version = f.version(file.Content, file.Version.Inode)
}

// SetLinkCount sets the hard-link count of name, so tests can exercise the
// multiply-linked rejection without creating real links.
func (f *FS) SetLinkCount(name string, n uint64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if file := f.files[canonical(name)]; file != nil {
		file.LinkCount = n
	}
}

// Content returns the stored content of name.
func (f *FS) Content(name string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	file := f.files[canonical(name)]
	if file == nil {
		return "", false
	}
	return file.Content, true
}

// Names returns the stored paths, sorted.
func (f *FS) Names() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	names := make([]string, 0, len(f.files))
	for name := range f.files {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// FailNext makes the next mutating call fail with code.
func (f *FS) FailNext(code string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failure = code
}

// Calls returns the operation names invoked so far.
func (f *FS) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// Target implements execution.Filesystem.
func (f *FS) Target() string { return f.TargetName }

// Close implements execution.Filesystem.
func (f *FS) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	return nil
}

// ReadFile implements execution.Filesystem.
func (f *FS) ReadFile(ctx context.Context, name string, maxBytes int64) (execution.FileSnapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "read "+name)
	file := f.files[canonical(name)]
	if file == nil {
		return execution.FileSnapshot{}, execution.NewFSError(execution.FSNotFound, name, "file not found")
	}
	if limit := f.limit(maxBytes); limit > 0 && int64(len(file.Content)) > limit {
		return execution.FileSnapshot{}, execution.NewFSError(execution.FSFileLimitExceeded, name, fmt.Sprintf("file exceeds %d bytes", limit))
	}
	return execution.FileSnapshot{
		Stat:    f.stat(name, file),
		Content: []byte(file.Content),
	}, nil
}

// CreateFile implements execution.Filesystem.
func (f *FS) CreateFile(ctx context.Context, name string, content []byte) (execution.FileStat, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "create "+name)
	if err := f.takeFailure(name); err != nil {
		return execution.FileStat{}, err
	}
	key := canonical(name)
	if f.files[key] != nil {
		return execution.FileStat{}, execution.NewFSError(execution.FSAlreadyExists, name, "file exists")
	}
	f.put(key, string(content))
	return f.stat(key, f.files[key]), nil
}

// ReplaceFile implements execution.Filesystem.
func (f *FS) ReplaceFile(ctx context.Context, name string, content []byte, expected execution.FileVersion) (execution.FileStat, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "replace "+name)
	if err := f.takeFailure(name); err != nil {
		return execution.FileStat{}, err
	}
	key := canonical(name)
	file := f.files[key]
	if file == nil {
		return execution.FileStat{}, execution.NewFSError(execution.FSNotFound, name, "file not found")
	}
	if !file.Version.Equal(expected) {
		return execution.FileStat{}, execution.NewFSError(execution.FSStaleRead, name, "file changed")
	}
	if file.LinkCount > 1 {
		return execution.FileStat{}, execution.NewFSError(execution.FSHardLinksNotAllowed, name, "file has multiple hard links")
	}
	f.put(key, string(content))
	return f.stat(key, f.files[key]), nil
}

// takeFailure consumes a pending injected failure.
func (f *FS) takeFailure(name string) error {
	if f.failure == "" {
		return nil
	}
	code := f.failure
	f.failure = ""
	return execution.NewFSError(code, name, "injected failure")
}

// put stores content with a fresh version. The caller holds the lock.
func (f *FS) put(name, content string) {
	key := canonical(name)
	file := f.files[key]
	inode := f.nextID + 1
	if file != nil {
		inode = file.Version.Inode
	}
	f.nextID++
	stored := &File{Content: content, Mode: 0o644, LinkCount: 1}
	stored.Version = f.version(content, inode)
	if file != nil {
		stored.Mode = file.Mode
	}
	f.files[key] = stored
}

// version builds a version for content with a monotonically increasing clock.
func (f *FS) version(content string, inode uint64) execution.FileVersion {
	if inode == 0 {
		inode = f.nextID + 1
	}
	sum := sha256.Sum256([]byte(content))
	return execution.FileVersion{
		Device:             1,
		Inode:              inode,
		Size:               int64(len(content)),
		ModTimeUnixNano:    int64(f.nextID),
		ChangeTimeUnixNano: int64(f.nextID),
		Digest:             hex.EncodeToString(sum[:]),
	}
}

func (f *FS) stat(name string, file *File) execution.FileStat {
	return execution.FileStat{
		Name:      canonical(name),
		Version:   file.Version,
		Mode:      file.Mode,
		LinkCount: file.LinkCount,
	}
}

func (f *FS) limit(maxBytes int64) int64 {
	if f.MaxBytes > 0 && (maxBytes <= 0 || f.MaxBytes < maxBytes) {
		return f.MaxBytes
	}
	return maxBytes
}

// canonical normalizes a test path the way a root-relative backend would.
func canonical(name string) string {
	cleaned := path.Clean(strings.TrimPrefix(name, "./"))
	if strings.HasPrefix(cleaned, "/") {
		cleaned = strings.TrimPrefix(cleaned, "/")
	}
	if cleaned == "" {
		return "."
	}
	return cleaned
}
