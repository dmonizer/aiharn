//go:build linux

package fscore

import (
	"golang.org/x/sys/unix"

	"aiharn/internal/execution"
)

// Replace atomically replaces an existing regular file if and only if its current
// version matches expected, preserving permission bits and ownership.
func (s *Service) Replace(name string, content []byte, expected execution.FileVersion) (execution.FileStat, error) {
	release, err := s.begin()
	if err != nil {
		return execution.FileStat{}, err
	}
	defer release()

	target, err := s.resolve(name)
	if err != nil {
		return execution.FileStat{}, err
	}
	defer target.close()

	return replaceFile(target, content, expected)
}

// replaceFile stages the new content next to the destination and renames it over
// the leaf. The leaf is never truncated, so a reader sees either the old or the
// new content in full.
func replaceFile(target *resolved, content []byte, expected execution.FileVersion) (execution.FileStat, error) {
	mode, uid, gid, err := preservedMetadata(target, expected)
	if err != nil {
		return execution.FileStat{}, err
	}

	staged, err := newStagedFile(target.dir, target.name)
	if err != nil {
		return execution.FileStat{}, err
	}
	defer func() { _ = staged.discard() }()

	if err := staged.write(content); err != nil {
		return execution.FileStat{}, err
	}
	if err := staged.setMode(mode); err != nil {
		return execution.FileStat{}, err
	}
	ownerChanged, err := staged.setOwner(uid, gid)
	if err != nil {
		return execution.FileStat{}, err
	}
	if ownerChanged {
		// chown(2) may clear the set-user-ID and set-group-ID bits, so restore
		// the preserved permission bits after an ownership change.
		if err := staged.setMode(mode); err != nil {
			return execution.FileStat{}, err
		}
	}
	if err := staged.sync(); err != nil {
		return execution.FileStat{}, err
	}
	if err := staged.close(); err != nil {
		return execution.FileStat{}, err
	}

	// Re-check immediately before publication: a concurrent writer since the
	// caller's read must not be overwritten.
	if err := requireExpected(target, expected); err != nil {
		return execution.FileStat{}, err
	}
	if err := publishReplace(target.dir, staged.name, target.leaf, target.name); err != nil {
		return execution.FileStat{}, err
	}
	if err := staged.discard(); err != nil {
		return execution.FileStat{}, err
	}
	if err := syncPublishedDir(target.dir, target.name); err != nil {
		return execution.FileStat{}, err
	}
	return publishedStat(target, content)
}

// preservedMetadata validates the current file against expected and returns the
// permission bits and ownership the replacement has to keep.
func preservedMetadata(target *resolved, expected execution.FileVersion) (mode, uid, gid uint32, err error) {
	validated, err := checkedLeaf(target, expected)
	if err != nil {
		return 0, 0, 0, err
	}
	// Open the leaf as well, so the metadata captured belongs to the object that
	// was validated rather than to whatever the name points at now.
	fd, opened, err := openRegular(target)
	if err != nil {
		return 0, 0, 0, err
	}
	_ = unix.Close(fd)
	if opened.Dev != validated.Dev || opened.Ino != validated.Ino {
		return 0, 0, 0, staleError(target.name)
	}
	return modeBits(opened), uint32(opened.Uid), uint32(opened.Gid), nil
}

// requireExpected re-validates the leaf immediately before publication.
func requireExpected(target *resolved, expected execution.FileVersion) error {
	_, err := checkedLeaf(target, expected)
	return err
}

// checkedLeaf enforces the replace preconditions: a single-linked regular file
// whose current version still matches expected.
func checkedLeaf(target *resolved, expected execution.FileVersion) (*unix.Stat_t, error) {
	stat, err := statLeaf(target.dir, target.leaf, target.name)
	if err != nil {
		return nil, err
	}
	if stat.Nlink != 1 {
		return nil, execution.NewFSError(execution.FSHardLinksNotAllowed, target.name,
			"file has other hard links; replacing it would change them")
	}
	if !versionMatches(stat, expected) {
		return nil, staleError(target.name)
	}
	return stat, nil
}
