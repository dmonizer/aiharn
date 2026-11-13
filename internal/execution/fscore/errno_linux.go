//go:build linux

package fscore

import (
	"errors"

	"golang.org/x/sys/unix"

	"aiharn/internal/execution"
)

// errnoIs reports whether err carries the given errno.
func errnoIs(err error, errno unix.Errno) bool { return errors.Is(err, errno) }

// ioError maps a syscall failure without a more specific meaning to a stable
// code. Permission failures get their own code; everything else is I/O.
func ioError(path, action string, err error) error {
	if errnoIs(err, unix.EACCES) || errnoIs(err, unix.EPERM) {
		return execution.NewFSError(execution.FSPermissionDenied, path, action+": permission denied")
	}
	return execution.NewFSError(execution.FSIOError, path, action+": "+errnoText(err))
}

// errnoText returns the generic description of a failing errno. It never
// carries file content or host paths.
func errnoText(err error) string {
	var errno unix.Errno
	if errors.As(err, &errno) {
		return errno.Error()
	}
	return "I/O failure"
}
