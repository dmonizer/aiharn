//go:build linux

package fscore

import (
	"golang.org/x/sys/unix"

	"aiharn/internal/execution"
)

// openDirFlags descend one component: read-only directory, never following a
// symlink, never leaking the descriptor into a child process.
const openDirFlags = unix.O_RDONLY | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC

// resolved names one confined entry: its canonical name, a handle to the
// directory it lives in, and the final component.
type resolved struct {
	name string // canonical root-relative name
	dir  int    // handle to the parent directory, -1 once closed
	leaf string // final component, relative to dir
}

// close releases the parent directory handle.
func (r *resolved) close() {
	if r.dir >= 0 {
		_ = unix.Close(r.dir)
		r.dir = -1
	}
}

// resolve validates name and opens its parent directory relative to the root
// handle. The caller must close the returned value.
func (s *Service) resolve(name string) (*resolved, error) {
	canonical, components, err := canonicalName(s.target, name)
	if err != nil {
		return nil, err
	}
	dir, err := s.parentHandle(components, canonical)
	if err != nil {
		return nil, err
	}
	return &resolved{name: canonical, dir: dir, leaf: components[len(components)-1]}, nil
}

// parentHandle returns a handle to the directory holding the final component. It
// walks one openat step per component, so the name is never re-resolved as a
// string and every step starts from the descriptor of the previous one.
func (s *Service) parentHandle(components []string, name string) (int, error) {
	dir, err := s.rootHandle(name)
	if err != nil {
		return -1, err
	}
	for _, component := range components[:len(components)-1] {
		next, err := openDirAt(dir, component, name)
		_ = unix.Close(dir)
		if err != nil {
			return -1, err
		}
		dir = next
	}
	return dir, nil
}

// rootHandle returns a fresh handle to the root directory that the caller owns.
func (s *Service) rootHandle(name string) (int, error) {
	fd, err := unix.Openat(s.rootFd, ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, ioError(name, "open root directory", err)
	}
	return fd, nil
}

// openDirAt descends into one component without following symlinks.
func openDirAt(dir int, component, name string) (int, error) {
	fd, err := unix.Openat(dir, component, openDirFlags, 0)
	if err != nil {
		return -1, componentError(dir, component, name, err)
	}
	return fd, nil
}

// componentError classifies a failed traversal. Linux reports ENOTDIR both for a
// symlink opened with O_NOFOLLOW and for a non-directory, so the entry itself
// decides which code applies.
func componentError(dir int, component, name string, err error) error {
	switch {
	case errnoIs(err, unix.ENOENT):
		return execution.NewFSError(execution.FSParentNotFound, name, "parent directory does not exist")
	case errnoIs(err, unix.EACCES), errnoIs(err, unix.EPERM):
		return execution.NewFSError(execution.FSPermissionDenied, name, "parent directory is not accessible")
	case errnoIs(err, unix.ELOOP):
		return symlinkError(name)
	case errnoIs(err, unix.ENOTDIR):
		if isSymlinkAt(dir, component) {
			return symlinkError(name)
		}
		return execution.NewFSError(execution.FSNotRegularFile, name, "path component is not a directory")
	default:
		return ioError(name, "open parent directory", err)
	}
}

// isSymlinkAt reports whether the entry is a symlink, without following it.
func isSymlinkAt(dir int, component string) bool {
	var stat unix.Stat_t
	if err := unix.Fstatat(dir, component, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return false
	}
	return isSymlink(&stat)
}
