//go:build unix

package library

import (
	"errors"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// sameDevice reports whether src and d are on the same file system, so a
// hard link or rename between them can work.
func sameDevice(src string, d *folder) bool {
	fa, err := os.Stat(src)
	if err != nil {
		return false
	}

	fb, err := d.root.Stat(".")
	if err != nil {
		return false
	}

	sa, ok1 := fa.Sys().(*syscall.Stat_t)
	sb, ok2 := fb.Sys().(*syscall.Stat_t)

	return ok1 && ok2 && sa.Dev == sb.Dev
}

// isCrossDevice reports whether a rename failed because it would cross
// file systems.
func isCrossDevice(err error) bool { return errors.Is(err, unix.EXDEV) }

// dirFile returns the folder itself, opened on first use.
func (d *folder) dirFile() (*os.File, error) {
	if d.dir == nil {
		f, err := d.root.Open(".")
		if err != nil {
			return nil, err
		}

		d.dir = f
	}

	return d.dir, nil
}

// at runs fn with a descriptor of the folder itself.
func (d *folder) at(fn func(fd int) error) error {
	f, err := d.dirFile()
	if err != nil {
		return err
	}

	rc, err := f.SyscallConn()
	if err != nil {
		return err
	}

	var opErr error
	if err := rc.Control(func(fd uintptr) { opErr = fn(int(fd)) }); err != nil {
		return err
	}

	return opErr
}

// sync makes a rename into d durable. Failures are ignored: the file itself
// is already synced. os.File.Sync is a full flush on macOS (F_FULLFSYNC),
// which a plain fsync is not.
func (d *folder) sync() {
	if f, err := d.dirFile(); err == nil {
		_ = f.Sync()
	}
}

// linkFrom hard-links src as name. It never replaces name.
func (d *folder) linkFrom(src, name string) error {
	err := d.at(func(fd int) error { return unix.Linkat(unix.AT_FDCWD, src, fd, name, 0) })
	if err != nil {
		return &os.LinkError{Op: "link", Old: src, New: name, Err: err}
	}

	return nil
}

// moveFrom renames src to name, replacing it.
func (d *folder) moveFrom(src, name string) error {
	err := d.at(func(fd int) error { return unix.Renameat(unix.AT_FDCWD, src, fd, name) })
	if err != nil {
		return &os.LinkError{Op: "rename", Old: src, New: name, Err: err}
	}

	return nil
}

// moveFromNoReplace renames src to name. It never replaces name.
func (d *folder) moveFromNoReplace(src, name string) error {
	err := d.at(func(fd int) error { return renameatNoReplace(unix.AT_FDCWD, src, fd, name) })
	if err != nil {
		return &os.LinkError{Op: "rename", Old: src, New: name, Err: err}
	}

	return nil
}

// renameNoReplace renames old to name within d. It never replaces name.
func (d *folder) renameNoReplace(old, name string) error {
	err := d.at(func(fd int) error { return renameatNoReplace(fd, old, fd, name) })
	if err != nil {
		return &os.LinkError{Op: "rename", Old: old, New: name, Err: err}
	}

	return nil
}

// renameatNoReplace renames oldname to newname, failing with EEXIST rather
// than replacing newname.
func renameatNoReplace(oldfd int, oldname string, newfd int, newname string) error {
	err := sysRenameatNoReplace(oldfd, oldname, newfd, newname)
	if !noReplaceUnsupported(err) {
		return err
	}

	return renameByLink(oldfd, oldname, newfd, newname)
}

// noReplaceUnsupported reports whether sysRenameatNoReplace failed because
// the kernel or the file system lacks it.
func noReplaceUnsupported(err error) bool {
	return errors.Is(err, unix.EINVAL) || errors.Is(err, unix.ENOSYS) ||
		errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.EOPNOTSUPP)
}

// renameByLink is renameatNoReplace for file systems without an atomic
// no-replace rename (NFS, some FUSE and SMB mounts). A hard link fails if
// newname exists; removing oldname then completes the rename.
func renameByLink(oldfd int, oldname string, newfd int, newname string) error {
	err := unix.Linkat(oldfd, oldname, newfd, newname, 0)
	if err == nil {
		_ = unix.Unlinkat(oldfd, oldname, 0)
		return nil
	}

	if !noHardLinks(err) {
		return err
	}

	// No hard links either (FAT, exFAT): check, then rename. Only here can a
	// file created in between still be replaced.
	var st unix.Stat_t
	if err := unix.Fstatat(newfd, newname, &st, unix.AT_SYMLINK_NOFOLLOW); err == nil {
		return unix.EEXIST
	}

	return unix.Renameat(oldfd, oldname, newfd, newname)
}

// noHardLinks reports whether a link failed because the file system has no
// hard links. Any other failure, such as a full disk, is returned as it is
// rather than giving up the no-replace promise.
func noHardLinks(err error) bool {
	return errors.Is(err, unix.EPERM) || errors.Is(err, unix.ENOSYS) ||
		errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.EOPNOTSUPP)
}
