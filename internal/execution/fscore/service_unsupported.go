//go:build !linux

package fscore

import "aiharn/internal/execution"

// Service is unavailable off Linux: confined traversal needs openat(2) semantics
// that this package implements for Linux only.
type Service struct {
	target string
}

// unsupported reports the platform limitation for name.
func unsupported(name string) error {
	return execution.NewFSError(execution.FSUnsupportedFilesystem, name, "local file tools require Linux")
}

// Open reports that the confined local filesystem requires Linux.
func Open(root string) (*Service, error) {
	return nil, unsupported(root)
}

// Target returns the canonical root identity the service would have used.
func (s *Service) Target() string { return s.target }

// Read reports that the confined local filesystem requires Linux.
func (s *Service) Read(name string, _ int64) (execution.FileSnapshot, error) {
	return execution.FileSnapshot{}, unsupported(name)
}

// Create reports that the confined local filesystem requires Linux.
func (s *Service) Create(name string, _ []byte) (execution.FileStat, error) {
	return execution.FileStat{}, unsupported(name)
}

// Replace reports that the confined local filesystem requires Linux.
func (s *Service) Replace(name string, _ []byte, _ execution.FileVersion) (execution.FileStat, error) {
	return execution.FileStat{}, unsupported(name)
}

// Close releases the service; nothing is held on this platform.
func (s *Service) Close() error { return nil }
