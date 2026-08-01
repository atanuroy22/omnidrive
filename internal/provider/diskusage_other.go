//go:build !windows && !linux && !darwin && !freebsd && !netbsd && !openbsd

package provider

// diskUsage is unavailable on this platform; an unknown quota is handled
// gracefully everywhere it is used.
func diskUsage(path string) (total, free int64, err error) {
	return 0, 0, nil
}
