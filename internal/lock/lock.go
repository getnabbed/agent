// Package lock provides an exclusive, process-level lock on the data
// directory so only one agent process uses a set of credentials at a time.
// Two processes refreshing the same rotating token would trip the cloud's
// reuse detection and revoke the agent.
//
// The lock is advisory and released by the OS when the process exits, so a
// crash never leaves a stale lock behind.
package lock

import (
	"errors"
	"os"
	"path/filepath"
)

// FileName is the lock file inside the data directory.
const FileName = "agent.lock"

// ErrLocked means another process holds the lock.
var ErrLocked = errors.New("data directory is in use by another agent process")

// Lock is a held lock.
type Lock struct{ f *os.File }

// Acquire takes the lock in dir without blocking.
func Acquire(dir string) (*Lock, error) {
	f, err := os.OpenFile(filepath.Join(dir, FileName), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}

	if err := lockFile(f); err != nil {
		_ = f.Close()
		return nil, err
	}

	return &Lock{f: f}, nil
}

// Release drops the lock.
func (l *Lock) Release() error {
	if l == nil || l.f == nil {
		return nil
	}

	_ = unlockFile(l.f)
	err := l.f.Close()
	l.f = nil

	return err
}
