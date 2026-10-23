//go:build !windows

package credstore

import (
	"log/slog"
	"os"
)

// PlatformProtector returns the default at-rest protector. On Unix there is
// no non-interactive OS secret store that works for daemons and containers,
// so credentials rely on owner-only permissions.
func PlatformProtector() Protector { return PlaintextProtector{} }

func platformProtectorByName(string) Protector { return nil }

func secureDir(dir string, log *slog.Logger) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	fi, err := os.Stat(dir)
	if err != nil {
		return err
	}
	if fi.Mode().Perm()&0o077 != 0 {
		if err := os.Chmod(dir, 0o700); err != nil {
			log.Warn("data dir is accessible by other users and could not be restricted",
				"dir", dir, "mode", fi.Mode().Perm().String(), "err", err)
		}
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
