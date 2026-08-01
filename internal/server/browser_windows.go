//go:build windows

package server

import "os/exec"

// OpenBrowser launches the user's default browser at url.
//
// Deliberately platform-split: the Android build must never link os/exec,
// because LookPath probes with faccessat2(2), which Android's seccomp policy
// answers with SIGSYS and kills the process. Windows and macOS have no such
// restriction, and Android is served by the no-op variant.
func OpenBrowser(url string) error {
	// rundll32 avoids `cmd /c start`, which mangles URLs containing & and
	// would drop the access token from the query string.
	return exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
}
