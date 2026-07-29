//go:build !windows

package htcfs

import "golang.org/x/sys/unix"

// diskFreeSpace reports the space available to this process and the
// filesystem's total, for the filesystem the path is on.
func diskFreeSpace(path string) (free, total int64, err error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, 0, err
	}
	return int64(st.Bavail) * int64(st.Bsize), int64(st.Blocks) * int64(st.Bsize), nil
}
