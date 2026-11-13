package execution

import (
	"context"
	"errors"
)

// Stable filesystem error codes shared by backends and callers. They are part
// of the model-facing contract: keep them stable, and add new codes rather than
// renaming existing ones.
const (
	// FSOutsideRoot reports a path that is absolute outside the root, empty, or
	// contains traversal components.
	FSOutsideRoot = "outside_root"
	// FSSymlinkNotAllowed reports a symlink in any path component, including the
	// root itself.
	FSSymlinkNotAllowed = "symlink_not_allowed"
	// FSNotRegularFile reports a directory, pipe, socket, or device.
	FSNotRegularFile = "not_regular_file"
	// FSNotFound reports a missing file.
	FSNotFound = "not_found"
	// FSParentNotFound reports a missing parent directory.
	FSParentNotFound = "parent_not_found"
	// FSAlreadyExists reports a destination that is already occupied.
	FSAlreadyExists = "already_exists"
	// FSFileLimitExceeded reports content larger than the caller's byte bound.
	FSFileLimitExceeded = "file_limit_exceeded"
	// FSStaleRead reports that the file no longer matches the expected version.
	FSStaleRead = "stale_read"
	// FSHardLinksNotAllowed reports a multiply linked file; replacing it would
	// silently change the other links.
	FSHardLinksNotAllowed = "hard_links_not_allowed"
	// FSPermissionDenied reports a host permission failure.
	FSPermissionDenied = "permission_denied"
	// FSIOError reports any other host I/O failure before publication.
	FSIOError = "io_error"
	// FSUnsupportedFilesystem reports a backend that cannot uphold the safety
	// contract (missing helper, unsupported platform or primitive).
	FSUnsupportedFilesystem = "unsupported_filesystem"
	// FSOutcomeUnknown reports a mutation whose commit state cannot be
	// determined (for example a disconnect during publication). Callers must
	// re-read before retrying and must not assume rollback.
	FSOutcomeUnknown = "outcome_unknown"
)

// FSError is a filesystem failure with a stable machine-readable code. Path is
// the root-relative (or supplied) name that failed; it never carries content.
type FSError struct {
	Code    string
	Message string
	Path    string
}

// NewFSError returns an FSError with the given code and message.
func NewFSError(code, path, message string) *FSError {
	return &FSError{Code: code, Path: path, Message: message}
}

// Error implements error.
func (e *FSError) Error() string {
	if e.Path == "" {
		return e.Code + ": " + e.Message
	}
	return e.Code + " (" + e.Path + "): " + e.Message
}

// FSECode returns the stable code of err, or "" when err carries no code. An
// unwrapped non-FS error is a generic I/O failure; callers map "" to FSIOError.
func FSECode(err error) string {
	var fe *FSError
	if errors.As(err, &fe) {
		return fe.Code
	}
	return ""
}

// IsFSECode reports whether err carries the given code.
func IsFSECode(err error, code string) bool { return FSECode(err) == code }

// FileVersion identifies one observed state of a regular file. It combines
// host identity metadata with a digest of the whole content, because metadata
// alone (second-resolution timestamps on some filesystems) cannot prove that
// content is unchanged.
type FileVersion struct {
	Device             uint64 `json:"device"`
	Inode              uint64 `json:"inode"`
	Size               int64  `json:"size"`
	ModTimeUnixNano    int64  `json:"mtime_ns"`
	ChangeTimeUnixNano int64  `json:"ctime_ns"`
	// Digest is the lowercase hex SHA-256 of the full file content.
	Digest string `json:"digest"`
}

// Equal reports whether two versions describe the same observed content. It is
// deliberately conservative: a re-written but byte-identical file is a
// different version.
func (v FileVersion) Equal(o FileVersion) bool {
	return v.Device == o.Device && v.Inode == o.Inode && v.Size == o.Size &&
		v.ModTimeUnixNano == o.ModTimeUnixNano && v.ChangeTimeUnixNano == o.ChangeTimeUnixNano &&
		v.Digest == o.Digest
}

// FileStat is the metadata of a regular file beneath a backend root. Name is
// the canonical root-relative path (slash separated, cleaned).
type FileStat struct {
	Name      string      `json:"name"`
	Version   FileVersion `json:"version"`
	Mode      uint32      `json:"mode"` // permission bits only
	LinkCount uint64      `json:"link_count"`
	UID       uint32      `json:"uid"`
	GID       uint32      `json:"gid"`
}

// FileSnapshot is a bounded, complete read of a regular file.
type FileSnapshot struct {
	Stat    FileStat
	Content []byte
}

// Filesystem is the channel-bound structured filesystem capability. It is
// separate from Session: it never runs shell commands and never interpolates
// names into a command line. Every method resolves name beneath the root with
// component-aware containment and no-follow semantics, and reports failures as
// *FSError.
//
// Implementations must reject symlinks in every component, non-regular files,
// and escapes; bound reads rather than trusting a pre-read size check; create
// new files atomically without replacement; and replace files atomically while
// preserving permissions and ownership (or refuse when metadata cannot be
// preserved). A root-relative path that is already clean is the canonical form;
// absolute paths inside the root are accepted and canonicalized.
type Filesystem interface {
	// Target identifies the execution target (channel kind, host, and canonical
	// root). Two backends with the same Target may alias the same files.
	Target() string

	// ReadFile returns the whole file content plus its version. Content larger
	// than maxBytes yields file_limit_exceeded without returning partial data.
	ReadFile(ctx context.Context, name string, maxBytes int64) (FileSnapshot, error)

	// CreateFile atomically publishes a new regular file. It fails with
	// already_exists when any entry occupies the destination, including a
	// dangling symlink, and with parent_not_found when the parent is missing. It
	// never creates parent directories and never replaces an existing entry.
	CreateFile(ctx context.Context, name string, content []byte) (FileStat, error)

	// ReplaceFile atomically replaces an existing regular file, preserving its
	// permission bits and ownership, if and only if the current version matches
	// expected. It fails with stale_read on a version mismatch and
	// hard_links_not_allowed for a multiply linked file. Publication is a
	// same-directory rename of a fully written staging file, so a reader never
	// observes truncated content.
	ReplaceFile(ctx context.Context, name string, content []byte, expected FileVersion) (FileStat, error)

	// Close releases the backend (for example a helper connection).
	Close() error
}

// FilesystemProvider is implemented by transports that can supply a confined
// filesystem backend for a root on their execution host. Transports that cannot
// uphold the contract either do not implement this interface or return an error
// with code FSUnsupportedFilesystem.
type FilesystemProvider interface {
	NewFilesystem(ctx context.Context, root string) (Filesystem, error)
}
