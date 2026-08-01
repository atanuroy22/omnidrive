//go:build darwin

package server

import "os/exec"

// OpenBrowser launches the user's default browser at url. See the Windows
// variant for why this is split per platform rather than shared.
func OpenBrowser(url string) error {
	return exec.Command("open", url).Start()
}
