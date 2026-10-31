//go:build windows

package library

import (
	"errors"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

// sameDevice reports whether src and d are on the same volume. Hard links
// and renames fail across volumes, which Place falls back from anyway.
func sameDevice(src string, d *folder) bool {
	return strings.EqualFold(filepath.VolumeName(src), filepath.VolumeName(d.root.Name()))
}

// isCrossDevice reports whether a rename failed because it would cross
// volumes.
func isCrossDevice(err error) bool { return errors.Is(err, windows.ERROR_NOT_SAME_DEVICE) }

// sync is a no-op on Windows, where renames are durable once the file is.
func (d *folder) sync() {}

// Windows has no link or rename relative to an open folder, so the calls
// below that bring a file into d use its path. Creating a directory
// junction needs no privileges, so someone who can write to the library can
// still swap a folder on that path for a junction between Place opening d
// and the file landing; only the Unix build closes that race. MoveFileEx
// without MOVEFILE_REPLACE_EXISTING never replaces a file.

func (d *folder) path(name string) string { return filepath.Join(d.root.Name(), name) }

// linkFrom hard-links src as name. It never replaces name.
func (d *folder) linkFrom(src, name string) error { return os.Link(src, d.path(name)) }

// moveFrom renames src to name, replacing it.
func (d *folder) moveFrom(src, name string) error { return os.Rename(src, d.path(name)) }

// moveFromNoReplace renames src to name. It never replaces name.
func (d *folder) moveFromNoReplace(src, name string) error {
	return moveFileNoReplace(src, d.path(name))
}

// renameNoReplace renames old to name within d. It never replaces name.
func (d *folder) renameNoReplace(old, name string) error {
	return moveFileNoReplace(d.path(old), d.path(name))
}

func moveFileNoReplace(from, to string) error {
	f, err := windows.UTF16PtrFromString(from)
	if err != nil {
		return &os.LinkError{Op: "rename", Old: from, New: to, Err: err}
	}

	t, err := windows.UTF16PtrFromString(to)
	if err != nil {
		return &os.LinkError{Op: "rename", Old: from, New: to, Err: err}
	}

	if err := windows.MoveFileEx(f, t, 0); err != nil {
		return &os.LinkError{Op: "rename", Old: from, New: to, Err: err}
	}

	return nil
}
