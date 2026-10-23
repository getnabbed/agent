//go:build !windows

package credstore

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"syscall"
)

// PlatformProtector returns the default at-rest protector. On Unix there is
// no non-interactive OS secret store that works for daemons and containers,
// so credentials rely on owner-only permissions.
func PlatformProtector() Protector { return PlaintextProtector{} }

func platformProtectorByName(string) Protector { return nil }

// secureDir creates dir owner-only, or checks an existing one. The
// directory must be owned by the agent's own uid: anyone else who owns it
// can replace the credentials whatever its mode says.
func secureDir(dir string, _ *slog.Logger) error {
	created := false

	if _, err := os.Lstat(dir); errors.Is(err, fs.ErrNotExist) {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}

		created = true
	}

	fi, err := os.Stat(dir)
	if err != nil {
		return err
	}

	if !fi.IsDir() {
		return fmt.Errorf("data dir %s is not a directory", dir)
	}

	if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Geteuid() {
		return fmt.Errorf("data dir %s is owned by uid %d, but the agent runs as uid %d; chown it or choose another directory",
			dir, st.Uid, os.Geteuid())
	}

	if fi.Mode().Perm()&0o077 == 0 {
		return nil
	}

	if !created {
		if err := checkDedicated(dir); err != nil {
			return err
		}
	}

	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("data dir %s is accessible by other users and could not be restricted: %w", dir, err)
	}

	return nil
}

func checkFileMode(path string, log *slog.Logger) {
	fi, err := os.Stat(path)
	if err == nil && fi.Mode().Perm()&0o077 != 0 {
		log.Warn("credentials file is readable by other users; run chmod 600 on it",
			"path", path, "mode", fi.Mode().Perm().String())
	}
}

func isChmodUnsupported(error) bool { return false }

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}

	defer func() { _ = d.Close() }()

	return d.Sync()
}

func renameReplace(from, to string) error { return os.Rename(from, to) }
