//go:build !windows && !darwin

package server

import "errors"

// OpenBrowser is not available here.
//
// This variant covers Linux, and therefore Android, where launching anything
// is off-limits: os/exec.LookPath probes candidates with faccessat2(2), which
// Android's seccomp policy answers with SIGSYS — killing the process rather
// than returning an error. Keeping os/exec out of this build entirely is the
// only reliable defence, so desktop Linux forgoes the convenience too.
func OpenBrowser(url string) error {
	return errors.New("opening a browser is not supported on this platform")
}
