//go:build !windows

package library

import "syscall"

// umask reads the process umask, which applies to created folders.
func umask() uint32 {
	m := syscall.Umask(0)
	syscall.Umask(m)

	return uint32(m)
}
