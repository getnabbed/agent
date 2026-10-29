//go:build unix

package hoststats

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// DiskUsage returns the space on the file system holding path. Free is
// what an unprivileged user may still write, excluding reserved blocks.
func DiskUsage(path string) (Usage, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return Usage{}, fmt.Errorf("statfs %s: %w", path, err)
	}

	bs := uint64(st.Bsize) //nolint:unconvert // the field types vary by OS

	return Usage{Free: uint64(st.Bavail) * bs, Total: uint64(st.Blocks) * bs}, nil //nolint:unconvert // the field types vary by OS
}
