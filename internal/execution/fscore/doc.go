// Package fscore implements a root-confined, race-resistant filesystem over one
// directory handle.
//
// A Service opens its root directory once and resolves every name against that
// handle with openat(2), so a name that was checked is never re-resolved as a
// string and no component can escape the root through a symlink or a rename.
// Names are validated component by component; symlinks, non-regular files,
// hard-linked replace targets, and escaping names are refused with stable
// execution.FSError codes. Mutations are staged in the destination directory and
// published with linkat(2) (create) or renameat(2) (replace), so a reader never
// observes partial content and a failed mutation never truncates a file.
//
// Support is Linux-only: the implementation relies on Linux openat(2) semantics.
// On other platforms Open reports execution.FSUnsupportedFilesystem.
package fscore
