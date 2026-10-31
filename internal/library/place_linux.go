package library

import "golang.org/x/sys/unix"

// sysRenameatNoReplace renames oldname to newname atomically unless
// newname exists.
func sysRenameatNoReplace(oldfd int, oldname string, newfd int, newname string) error {
	return unix.Renameat2(oldfd, oldname, newfd, newname, unix.RENAME_NOREPLACE)
}
