//go:build !linux

package telegram

// diskFreeBytes is not implemented on this platform; -1 means unknown.
func diskFreeBytes(string) int64 { return -1 }
