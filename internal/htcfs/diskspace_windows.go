package htcfs

import (
	"golang.org/x/sys/windows"
)

// diskFreeSpace reports the space available to this process and the volume's
// total, for the volume the path is on.
func diskFreeSpace(path string) (free, total int64, err error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, 0, err
	}
	var avail, totalBytes, totalFree uint64
	if err := windows.GetDiskFreeSpaceEx(p, &avail, &totalBytes, &totalFree); err != nil {
		return 0, 0, err
	}
	return int64(avail), int64(totalBytes), nil
}
