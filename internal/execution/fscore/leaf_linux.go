//go:build linux

package fscore

import (
	"golang.org/x/sys/unix"

	"aiharn/internal/execution"
)

// openLeafFlags open a leaf for reading: never follow a symlink, never block on a
// FIFO, never leak the descriptor.
const openLeafFlags = unix.O_RDONLY | unix.O_NOFOLLOW | unix.O_NONBLOCK | unix.O_CLOEXEC

// maxOpenAttempts bounds the retries needed when a leaf is re-bound to another
// object between the stat that validates it and the open that reads it.
const maxOpenAttempts = 3

// statLeaf validates the leaf without following it: symlinks and non-regular
// entries are refused before anything is opened.
func statLeaf(dir int, leaf, name string) (*unix.Stat_t, error) {
	var stat unix.Stat_t
	if err := unix.Fstatat(dir, leaf, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return nil, leafStatError(name, err)
	}
	if err := requireRegular(&stat, name); err != nil {
		return nil, err
	}
	return &stat, nil
}

// requireRegular refuses symlinks and non-regular entries.
func requireRegular(stat *unix.Stat_t, name string) error {
	if isSymlink(stat) {
		return symlinkError(name)
	}
	if !isRegular(stat) {
		return notRegularFileError(name)
	}
	return nil
}

// openRegular opens the validated leaf and confirms that the descriptor refers to
// the very entry the stat described, so a rename between check and open cannot
// substitute a different object.
func openRegular(target *resolved) (int, *unix.Stat_t, error) {
	for attempt := 0; attempt < maxOpenAttempts; attempt++ {
		validated, err := statLeaf(target.dir, target.leaf, target.name)
		if err != nil {
			return -1, nil, err
		}
		fd, err := unix.Openat(target.dir, target.leaf, openLeafFlags, 0)
		if err != nil {
			return -1, nil, leafOpenError(target.name, err)
		}
		opened, err := fstat(fd, target.name)
		if err == nil {
			err = requireRegular(opened, target.name)
		}
		if err != nil {
			_ = unix.Close(fd)
			return -1, nil, err
		}
		if opened.Dev == validated.Dev && opened.Ino == validated.Ino {
			return fd, opened, nil
		}
		// The name was re-bound to another object; validate and open again.
		_ = unix.Close(fd)
	}
	return -1, nil, execution.NewFSError(execution.FSStaleRead, target.name,
		"file is being replaced concurrently")
}

// fstat reads the metadata of an open descriptor.
func fstat(fd int, name string) (*unix.Stat_t, error) {
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return nil, ioError(name, "stat file", err)
	}
	return &stat, nil
}

// leafStatError classifies a failed no-follow stat of the leaf.
func leafStatError(name string, err error) error {
	switch {
	case errnoIs(err, unix.ENOENT):
		return notFoundError(name)
	case errnoIs(err, unix.EACCES), errnoIs(err, unix.EPERM):
		return execution.NewFSError(execution.FSPermissionDenied, name, "file is not accessible")
	case errnoIs(err, unix.ENOTDIR):
		return notFoundError(name)
	default:
		return ioError(name, "stat file", err)
	}
}

// leafOpenError classifies a failed open of the leaf.
func leafOpenError(name string, err error) error {
	switch {
	case errnoIs(err, unix.ELOOP):
		return symlinkError(name)
	case errnoIs(err, unix.ENOENT):
		return notFoundError(name)
	case errnoIs(err, unix.EACCES), errnoIs(err, unix.EPERM):
		return execution.NewFSError(execution.FSPermissionDenied, name, "file is not readable")
	case errnoIs(err, unix.EISDIR), errnoIs(err, unix.ENOTDIR):
		return notRegularFileError(name)
	default:
		return ioError(name, "open file", err)
	}
}
