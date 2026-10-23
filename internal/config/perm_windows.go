//go:build windows

package config

// worldReadable is not checked on Windows, where access is governed by the
// file's ACL rather than mode bits.
func worldReadable(string) bool { return false }
