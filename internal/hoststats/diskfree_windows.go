//go:build windows

package hoststats

import (
	"fmt"

	"golang.org/x/sys/windows"
)

// DiskUsage returns the space on the volume holding path. Free is what the
// agent's user may still write, honouring disk quotas.
func DiskUsage(path string) (Usage, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return Usage{}, fmt.Errorf("disk usage of %s: %w", path, err)
	}

	var avail, total, free uint64
	if err := windows.GetDiskFreeSpaceEx(p, &avail, &total, &free); err != nil {
		return Usage{}, fmt.Errorf("disk usage of %s: %w", path, err)
	}

	return Usage{Free: avail, Total: total}, nil
}
