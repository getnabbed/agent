//go:build unix

package library

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

// The fallback for file systems without an atomic no-replace rename keeps
// the same promise.
func TestRenameByLinkNeverReplaces(t *testing.T) {
	dir := t.TempDir()
	d := openTestFolder(t, dir)

	for name, data := range map[string]string{"a": "new", "b": "old"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	err := d.at(func(fd int) error { return renameByLink(fd, "a", fd, "b") })
	if !errors.Is(err, unix.EEXIST) {
		t.Fatalf("got %v, want EEXIST", err)
	}

	if got, _ := os.ReadFile(filepath.Join(dir, "b")); string(got) != "old" {
		t.Fatalf("got %q, want the existing file kept", got)
	}

	if err := d.at(func(fd int) error { return renameByLink(fd, "a", fd, "c") }); err != nil {
		t.Fatal(err)
	}

	if got, _ := os.ReadFile(filepath.Join(dir, "c")); string(got) != "new" {
		t.Fatalf("got %q after renaming", got)
	}

	if _, err := os.Stat(filepath.Join(dir, "a")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("got %v, want the old name gone", err)
	}
}
