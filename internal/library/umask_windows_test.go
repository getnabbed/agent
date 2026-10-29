//go:build windows

package library

// umask is zero on Windows, which has no umask; tests skip mode checks there.
func umask() uint32 { return 0 }
