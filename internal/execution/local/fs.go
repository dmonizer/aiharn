package local

import (
	"context"

	"aiharn/internal/execution"
	"aiharn/internal/execution/fscore"
)

// compile-time assertion
var _ execution.FilesystemProvider = (*Transport)(nil)

// NewFilesystem returns a confined filesystem backend for root on this machine.
// The backend resolves every name beneath root with no-follow semantics, so it
// cannot reach files outside root through a symlink or a path traversal.
func (t *Transport) NewFilesystem(ctx context.Context, root string) (execution.Filesystem, error) {
	if err := contextError(ctx, root); err != nil {
		return nil, err
	}
	service, err := fscore.Open(root)
	if err != nil {
		return nil, err
	}
	return &filesystem{service: service}, nil
}

// filesystem adapts a root-confined fscore.Service to execution.Filesystem.
type filesystem struct {
	service *fscore.Service
}

var _ execution.Filesystem = (*filesystem)(nil)

// Target identifies the backend as the local channel plus the canonical root.
func (f *filesystem) Target() string { return "local:" + f.service.Target() }

// ReadFile returns the whole content of name plus its version.
func (f *filesystem) ReadFile(ctx context.Context, name string, maxBytes int64) (execution.FileSnapshot, error) {
	if err := contextError(ctx, name); err != nil {
		return execution.FileSnapshot{}, err
	}
	return f.service.Read(name, maxBytes)
}

// CreateFile atomically publishes a new regular file.
func (f *filesystem) CreateFile(ctx context.Context, name string, content []byte) (execution.FileStat, error) {
	if err := contextError(ctx, name); err != nil {
		return execution.FileStat{}, err
	}
	return f.service.Create(name, content)
}

// ReplaceFile atomically replaces a file whose version still matches expected.
func (f *filesystem) ReplaceFile(ctx context.Context, name string, content []byte, expected execution.FileVersion) (execution.FileStat, error) {
	if err := contextError(ctx, name); err != nil {
		return execution.FileStat{}, err
	}
	return f.service.Replace(name, content, expected)
}

// Close releases the root handle of the backend.
func (f *filesystem) Close() error { return f.service.Close() }

// contextError reports a finished context as a filesystem I/O failure.
func contextError(ctx context.Context, name string) error {
	if err := ctx.Err(); err != nil {
		return execution.NewFSError(execution.FSIOError, name, "context is done: "+err.Error())
	}
	return nil
}
