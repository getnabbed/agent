//go:build !windows

package config

import "os"

// worldReadable reports whether users other than the owner can read path.
func worldReadable(path string) bool {
	fi, err := os.Stat(path)

	return err == nil && fi.Mode().Perm()&0o044 != 0
}
