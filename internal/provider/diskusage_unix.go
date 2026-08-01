//go:build linux || darwin || freebsd || netbsd || openbsd

package provider

import "syscall"

// diskUsage reports total and free bytes for the filesystem holding path.
// Statfs is in the standard library for these platforms, so this needs no
// dependency and no cgo.
func diskUsage(path string) (total, free int64, err error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0, err
	}
	bsize := int64(st.Bsize)
	// Bavail, not Bfree: the latter counts blocks reserved for root, which an
	// app can never actually use.
	return int64(st.Blocks) * bsize, int64(st.Bavail) * bsize, nil
}
