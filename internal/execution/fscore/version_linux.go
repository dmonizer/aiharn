//go:build linux

package fscore

import (
	"golang.org/x/sys/unix"

	"aiharn/internal/execution"
)

// permissionMask keeps the POSIX permission bits of a mode.
const permissionMask = 0o7777

// nanoPerSecond converts whole seconds of a timestamp to nanoseconds.
const nanoPerSecond = 1_000_000_000

// fileStatOf builds the public metadata for name from a stat of a regular file.
func fileStatOf(name string, stat *unix.Stat_t, digest string) execution.FileStat {
	return execution.FileStat{
		Name:      name,
		Version:   versionOf(stat, digest),
		Mode:      modeBits(stat),
		LinkCount: uint64(stat.Nlink),
		UID:       uint32(stat.Uid),
		GID:       uint32(stat.Gid),
	}
}

// versionOf extracts the host identity of a file plus the digest the caller
// computed for its content.
func versionOf(stat *unix.Stat_t, digest string) execution.FileVersion {
	return execution.FileVersion{
		Device:             uint64(stat.Dev),
		Inode:              uint64(stat.Ino),
		Size:               int64(stat.Size),
		ModTimeUnixNano:    timeNano(stat.Mtim),
		ChangeTimeUnixNano: timeNano(stat.Ctim),
		Digest:             digest,
	}
}

// versionMatches reports whether the observed metadata still matches the version
// the caller read. The digest is not compared: it describes content this backend
// did not hash, and metadata is what a race can be detected with here.
func versionMatches(stat *unix.Stat_t, expected execution.FileVersion) bool {
	return uint64(stat.Dev) == expected.Device &&
		uint64(stat.Ino) == expected.Inode &&
		int64(stat.Size) == expected.Size &&
		timeNano(stat.Mtim) == expected.ModTimeUnixNano &&
		timeNano(stat.Ctim) == expected.ChangeTimeUnixNano
}

// timeNano converts a filesystem timestamp to Unix nanoseconds.
func timeNano(ts unix.Timespec) int64 {
	return int64(ts.Sec)*nanoPerSecond + int64(ts.Nsec)
}

// modeBits returns only the permission bits of a mode.
func modeBits(stat *unix.Stat_t) uint32 { return stat.Mode & permissionMask }

// isRegular reports whether the entry is a regular file.
func isRegular(stat *unix.Stat_t) bool { return stat.Mode&unix.S_IFMT == unix.S_IFREG }

// isDir reports whether the entry is a directory.
func isDir(stat *unix.Stat_t) bool { return stat.Mode&unix.S_IFMT == unix.S_IFDIR }

// isSymlink reports whether the entry is a symbolic link.
func isSymlink(stat *unix.Stat_t) bool { return stat.Mode&unix.S_IFMT == unix.S_IFLNK }
