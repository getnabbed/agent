//go:build !windows

package library

import (
	"os"
	"syscall"
)

// sameDevice reports whether a and b are on the same file system, so a
// hard link or rename between them can work.
func sameDevice(a, b string) bool {
	fa, err := os.Stat(a)
	if err != nil {
		return false
	}

	fb, err := os.Stat(b)
	if err != nil {
		return false
	}

	sa, ok1 := fa.Sys().(*syscall.Stat_t)
	sb, ok2 := fb.Sys().(*syscall.Stat_t)

	return ok1 && ok2 && sa.Dev == sb.Dev
}

// syncDir makes a rename into dir durable. Failures are ignored: the file
// itself is already synced.
func syncDir(dir string) {
	f, err := os.Open(dir)
	if err != nil {
		return
	}

	_ = f.Sync()
	_ = f.Close()
}
