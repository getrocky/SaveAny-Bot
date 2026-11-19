//go:build !linux

package cachegc

// DiskFreeBytes is not implemented on this platform; -1 means unknown.
func DiskFreeBytes(string) int64 { return -1 }
