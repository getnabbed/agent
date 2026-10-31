//go:build unix && !linux && !darwin

package library

import "golang.org/x/sys/unix"

// sysRenameatNoReplace is not available here; renameatNoReplace falls back
// to a hard link.
func sysRenameatNoReplace(int, string, int, string) error { return unix.ENOSYS }
