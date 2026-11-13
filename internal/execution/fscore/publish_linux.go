//go:build linux

package fscore

import (
	"golang.org/x/sys/unix"

	"aiharn/internal/execution"
)

// publishNew publishes the staging file as leaf without ever replacing an
// existing entry. linkat(2) is the only primitive that both creates and refuses
// to overwrite; a copy or rename fallback would weaken the guarantee, so an
// unsupported filesystem is reported instead.
func publishNew(dir int, stagingName, leaf, path string) error {
	if err := unix.Linkat(dir, stagingName, dir, leaf, 0); err != nil {
		switch {
		case errnoIs(err, unix.EEXIST):
			return execution.NewFSError(execution.FSAlreadyExists, path, "destination already exists")
		case errnoIs(err, unix.EOPNOTSUPP), errnoIs(err, unix.EPERM), errnoIs(err, unix.ENOSYS):
			return execution.NewFSError(execution.FSUnsupportedFilesystem, path,
				"filesystem does not support atomic creation")
		case errnoIs(err, unix.ENOENT), errnoIs(err, unix.ENOTDIR):
			return execution.NewFSError(execution.FSParentNotFound, path, "parent directory does not exist")
		default:
			return ioError(path, "publish file", err)
		}
	}
	return nil
}

// publishReplace atomically renames the staging file over leaf.
func publishReplace(dir int, stagingName, leaf, path string) error {
	if err := unix.Renameat(dir, stagingName, dir, leaf); err != nil {
		switch {
		case errnoIs(err, unix.EISDIR), errnoIs(err, unix.ENOTEMPTY):
			return notRegularFileError(path)
		case errnoIs(err, unix.ENOENT), errnoIs(err, unix.ENOTDIR):
			return notFoundError(path)
		case errnoIs(err, unix.EACCES), errnoIs(err, unix.EPERM):
			return execution.NewFSError(execution.FSPermissionDenied, path, "destination is not writable")
		default:
			return ioError(path, "publish file", err)
		}
	}
	return nil
}

// syncPublishedDir flushes the directory entry of a completed publication, so it
// is durable once this returns. A failure happens after the file is already
// visible, so the outcome is reported as unknown rather than as a failed write.
func syncPublishedDir(dir int, path string) error {
	if err := unix.Fsync(dir); err != nil {
		return execution.NewFSError(execution.FSOutcomeUnknown, path,
			"file was published but the directory could not be synced: "+errnoText(err))
	}
	return nil
}
