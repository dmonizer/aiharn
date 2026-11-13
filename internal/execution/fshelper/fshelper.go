// Package fshelper serves the fsproto filesystem protocol on a byte stream
// (stdin/stdout of a remote command). The aiharn `fs-helper` subcommand wires
// the real backend; the ssh transport drives the stream.
//
// The package deliberately declares the backend interface it needs instead of
// importing the concrete service, so the helper loop builds, tests, and ships
// independently of the filesystem implementation.
package fshelper

import (
	"aiharn/internal/execution"
)

// Backend is the filesystem capability the helper serves. It is the narrow
// slice of the local filesystem backend that the protocol needs; the canonical
// implementation (fscore.Service) satisfies it structurally.
type Backend interface {
	// Target identifies the backend and its canonical root.
	Target() string
	// Read returns the complete file content and its version. Content larger
	// than maxBytes yields a file_limit_exceeded error.
	Read(name string, maxBytes int64) (execution.FileSnapshot, error)
	// Create atomically publishes a new file without replacing any entry.
	Create(name string, content []byte) (execution.FileStat, error)
	// Replace atomically replaces a file whose current version matches expected.
	Replace(name string, content []byte, expected execution.FileVersion) (execution.FileStat, error)
	// Close releases the backend.
	Close() error
}

// Factory opens a backend confined to root. It is called once per session, after
// the init handshake supplies the root, and the returned backend is closed when
// the session ends.
type Factory func(root string) (Backend, error)
