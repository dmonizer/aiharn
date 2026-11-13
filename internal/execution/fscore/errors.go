package fscore

import "aiharn/internal/execution"

// outsideRootError reports a name that is empty, resolves outside the root, or
// carries a traversal component.
func outsideRootError(name string) error {
	return execution.NewFSError(execution.FSOutsideRoot, name,
		"path must stay inside the root and must not contain . or .. components")
}

// symlinkError reports a symbolic link met while resolving a name.
func symlinkError(name string) error {
	return execution.NewFSError(execution.FSSymlinkNotAllowed, name, "symbolic links are not allowed")
}

// notRegularFileError reports an entry that is not a regular file.
func notRegularFileError(name string) error {
	return execution.NewFSError(execution.FSNotRegularFile, name, "entry is not a regular file")
}

// notFoundError reports a missing file.
func notFoundError(name string) error {
	return execution.NewFSError(execution.FSNotFound, name, "file does not exist")
}

// staleError reports content that changed since the caller read it.
func staleError(name string) error {
	return execution.NewFSError(execution.FSStaleRead, name, "file changed since it was read")
}

// limitError reports content larger than the caller's byte bound.
func limitError(name string) error {
	return execution.NewFSError(execution.FSFileLimitExceeded, name,
		"file is larger than the requested byte limit")
}

// closedError reports use of a service that was already closed.
func closedError(target string) error {
	return execution.NewFSError(execution.FSIOError, target, "filesystem is closed")
}
