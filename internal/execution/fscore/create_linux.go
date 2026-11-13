//go:build linux

package fscore

import "aiharn/internal/execution"

// Create atomically publishes content as a new regular file named name. The
// parent directory must already exist and the destination must be free.
func (s *Service) Create(name string, content []byte) (execution.FileStat, error) {
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

	return createFile(target, content)
}

// createFile stages content in the destination directory and links it into place
// under the final name, cleaning the staging file up on every error path.
func createFile(target *resolved, content []byte) (execution.FileStat, error) {
	staged, err := newStagedFile(target.dir, target.name)
	if err != nil {
		return execution.FileStat{}, err
	}
	defer func() { _ = staged.discard() }()

	if err := writeStaged(staged, content); err != nil {
		return execution.FileStat{}, err
	}
	if err := publishNew(target.dir, staged.name, target.leaf, target.name); err != nil {
		return execution.FileStat{}, err
	}
	if err := dropStagingLink(staged); err != nil {
		return execution.FileStat{}, err
	}
	if err := syncPublishedDir(target.dir, target.name); err != nil {
		return execution.FileStat{}, err
	}
	return publishedStat(target, content)
}

// publishedStat re-reads the published entry so the reported metadata describes
// the file as it now exists on disk.
func publishedStat(target *resolved, content []byte) (execution.FileStat, error) {
	stat, err := statLeaf(target.dir, target.leaf, target.name)
	if err != nil {
		return execution.FileStat{}, err
	}
	return fileStatOf(target.name, stat, digestHex(content)), nil
}
