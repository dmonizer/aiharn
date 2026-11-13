//go:build linux

package fscore

import (
	"crypto/rand"
	"encoding/hex"

	"golang.org/x/sys/unix"

	"aiharn/internal/execution"
)

// stagingMode is the creation mode of a staging file; the umask still applies, so
// ownership and permission bits are set explicitly afterwards when they matter.
const stagingMode = 0o666

// stagingPrefix marks the temporary files this package creates, so leftovers are
// recognisable inside a destination directory.
const stagingPrefix = ".aiharn-tmp-"

// stagingRandomBytes is the entropy in a staging file name.
const stagingRandomBytes = 8

// stagedFile is a temporary file inside the destination directory that holds the
// complete new content. It exists only to be published under the final name, so a
// reader never observes partial content and the destination is never truncated.
type stagedFile struct {
	dirFd int    // destination directory handle, owned by the caller
	fd    int    // open file descriptor, -1 once closed
	name  string // temporary name relative to dirFd, "" once removed
	path  string // canonical destination name, used for error reporting
}

// newStagedFile creates an exclusive staging file in dirFd.
func newStagedFile(dirFd int, path string) (*stagedFile, error) {
	name, err := stagingName()
	if err != nil {
		return nil, execution.NewFSError(execution.FSIOError, path, "cannot generate a temporary file name")
	}
	fd, err := unix.Openat(dirFd, name,
		unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, stagingMode)
	if err != nil {
		return nil, ioError(path, "create temporary file", err)
	}
	return &stagedFile{dirFd: dirFd, fd: fd, name: name, path: path}, nil
}

// stagingName returns an unguessable staging name inside a destination directory.
func stagingName() (string, error) {
	var random [stagingRandomBytes]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	return stagingPrefix + hex.EncodeToString(random[:]), nil
}

// write writes all of content, retrying short and interrupted writes.
func (f *stagedFile) write(content []byte) error {
	for len(content) > 0 {
		n, err := unix.Write(f.fd, content)
		if err != nil {
			if errnoIs(err, unix.EINTR) {
				continue
			}
			return ioError(f.path, "write temporary file", err)
		}
		if n <= 0 {
			return execution.NewFSError(execution.FSIOError, f.path, "write temporary file: no progress")
		}
		content = content[n:]
	}
	return nil
}

// sync flushes the staged content and metadata to stable storage.
func (f *stagedFile) sync() error {
	if err := unix.Fsync(f.fd); err != nil {
		return ioError(f.path, "sync temporary file", err)
	}
	return nil
}

// setMode applies the permission bits the published file must carry.
func (f *stagedFile) setMode(mode uint32) error {
	if err := unix.Fchmod(f.fd, mode); err != nil {
		return ioError(f.path, "preserve file mode", err)
	}
	return nil
}

// setOwner copies the original owner onto the staging file and reports whether
// the owner actually changed. It fails with permission_denied when the owner
// cannot be preserved, because a silent ownership change would widen or break
// access to the file.
func (f *stagedFile) setOwner(uid, gid uint32) (changed bool, err error) {
	stat, err := fstat(f.fd, f.path)
	if err != nil {
		return false, err
	}
	if uint32(stat.Uid) == uid && uint32(stat.Gid) == gid {
		return false, nil
	}
	if err := unix.Fchown(f.fd, int(uid), int(gid)); err != nil {
		return false, execution.NewFSError(execution.FSPermissionDenied, f.path,
			"cannot preserve file ownership")
	}
	return true, nil
}

// close releases the staging descriptor.
func (f *stagedFile) close() error {
	if f.fd < 0 {
		return nil
	}
	fd := f.fd
	f.fd = -1
	if err := unix.Close(fd); err != nil {
		return ioError(f.path, "close temporary file", err)
	}
	return nil
}

// discard removes the temporary name. It is safe to call repeatedly and treats
// an already removed name as success.
func (f *stagedFile) discard() error {
	if f.name == "" {
		return nil
	}
	name := f.name
	f.name = ""
	if err := unix.Unlinkat(f.dirFd, name, 0); err != nil && !errnoIs(err, unix.ENOENT) {
		return ioError(f.path, "remove temporary file", err)
	}
	return nil
}

// writeStaged fills tmp with content and flushes it to stable storage.
func writeStaged(tmp *stagedFile, content []byte) error {
	if err := tmp.write(content); err != nil {
		return err
	}
	if err := tmp.sync(); err != nil {
		return err
	}
	return tmp.close()
}

// dropStagingLink removes the staging name after publication. The published name
// must stay; a leftover staging name would be a second link to the new content.
func dropStagingLink(tmp *stagedFile) error {
	if err := tmp.discard(); err != nil {
		return execution.NewFSError(execution.FSOutcomeUnknown, tmp.path,
			"file was published but its temporary link could not be removed")
	}
	return nil
}
