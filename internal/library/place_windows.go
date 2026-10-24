//go:build windows

package library

import (
	"path/filepath"
	"strings"
)

// sameDevice reports whether a and b are on the same volume. Hard links
// and renames fail across volumes, which Place falls back from anyway.
func sameDevice(a, b string) bool {
	return strings.EqualFold(filepath.VolumeName(a), filepath.VolumeName(b))
}

// syncDir is a no-op on Windows, where renames are durable once the file is.
func syncDir(string) {}
