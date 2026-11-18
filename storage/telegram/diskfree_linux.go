//go:build linux

package telegram

import "syscall"

// diskFreeBytes returns the bytes available to unprivileged users at dir, or
// -1 when unknown.
func diskFreeBytes(dir string) int64 {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return -1
	}
	return int64(st.Bavail) * int64(st.Bsize)
}
