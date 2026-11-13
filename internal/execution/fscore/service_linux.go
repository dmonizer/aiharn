//go:build linux

package fscore

import (
	"path/filepath"
	"sync"

	"golang.org/x/sys/unix"

	"aiharn/internal/execution"
)

// openRootFlags open the root directory without following a final symlink.
const openRootFlags = unix.O_RDONLY | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC

// Service is a root-confined filesystem bound to one open directory handle. It
// is safe for concurrent use: every operation resolves names from the root
// handle on its own descriptors and only Close touches shared state.
type Service struct {
	rootFd int    // handle to the canonical root directory, -1 once closed
	target string // canonical absolute root path

	mu     sync.RWMutex
	closed bool
}

// Open opens root for confined access. root must be absolute, existing, a
// directory, and not a symlink. Errors are *execution.FSError.
func Open(root string) (*Service, error) {
	target, err := canonicalRoot(root)
	if err != nil {
		return nil, err
	}
	fd, err := unix.Open(target, openRootFlags, 0)
	if err != nil {
		return nil, rootError(target, err)
	}
	stat, err := fstat(fd, target)
	if err != nil {
		_ = unix.Close(fd)
		return nil, err
	}
	if !isDir(stat) {
		_ = unix.Close(fd)
		return nil, execution.NewFSError(execution.FSNotRegularFile, target, "root is not a directory")
	}
	return &Service{rootFd: fd, target: target}, nil
}

// Target returns the canonical root identity, for example "/tmp/x".
func (s *Service) Target() string { return s.target }

// Close releases the root handle. Operations already in flight finish first;
// later operations fail with an I/O error. Close is idempotent.
func (s *Service) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	fd := s.rootFd
	s.rootFd = -1
	if err := unix.Close(fd); err != nil {
		return ioError(s.target, "close root", err)
	}
	return nil
}

// begin guards one operation against Close. The returned release function must
// be called when the operation finishes.
func (s *Service) begin() (func(), error) {
	s.mu.RLock()
	if s.closed {
		s.mu.RUnlock()
		return nil, closedError(s.target)
	}
	return s.mu.RUnlock, nil
}

// canonicalRoot validates root and returns its clean absolute form.
func canonicalRoot(root string) (string, error) {
	if !filepath.IsAbs(root) {
		return "", execution.NewFSError(execution.FSOutsideRoot, root, "root must be an absolute path")
	}
	return filepath.Clean(root), nil
}

// rootError classifies a failed open of the root directory.
func rootError(path string, err error) error {
	switch {
	case errnoIs(err, unix.ENOENT):
		return execution.NewFSError(execution.FSNotFound, path, "root does not exist")
	case errnoIs(err, unix.ELOOP):
		return symlinkError(path)
	case errnoIs(err, unix.ENOTDIR):
		// Linux reports ENOTDIR, not ELOOP, for some O_NOFOLLOW directory opens.
		if isSymlinkPath(path) {
			return symlinkError(path)
		}
		return execution.NewFSError(execution.FSNotRegularFile, path, "root is not a directory")
	case errnoIs(err, unix.EACCES), errnoIs(err, unix.EPERM):
		return execution.NewFSError(execution.FSPermissionDenied, path, "root is not accessible")
	default:
		return ioError(path, "open root", err)
	}
}

// isSymlinkPath reports whether path itself is a symlink, without following it.
// It only disambiguates error reporting; it never decides containment.
func isSymlinkPath(path string) bool {
	var stat unix.Stat_t
	if err := unix.Lstat(path, &stat); err != nil {
		return false
	}
	return isSymlink(&stat)
}
