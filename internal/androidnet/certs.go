package androidnet

import (
	"crypto/x509"
	"os"
	"path/filepath"
	"sync"
)

// Root CA loading for Android.
//
// Go's crypto/x509 only consults Android's trust store when the binary is
// built with GOOS=android — the paths are gated behind `goos.IsAndroid`. We
// build with GOOS=linux because a plain static ELF is far more portable
// (Termux, any distro, no NDK), which means the stock root loader finds
// nothing on a phone and every HTTPS call fails with:
//
//	x509: certificate signed by unknown authority
//
// Go's Android list is also out of date: since Android 14 the system roots
// live in a Conscrypt APEX module, not /system. So we scan every known
// location ourselves rather than relying on either.

var androidCertDirs = []string{
	"/apex/com.android.conscrypt/cacerts", // Android 14+ (APEX module)
	"/system/etc/security/cacerts",        // Android 7–13
	"/data/misc/keychain/certs-added",     // user-installed (legacy path)
	"/data/misc/user/0/cacerts-added",     // user-installed (current path)
}

var (
	rootsOnce   sync.Once
	rootsPool   *x509.CertPool
	rootsLoaded int
	rootsSource string
)

// RootCAs returns the certificate pool to use for outbound HTTPS.
//
// It returns nil when the platform's own roots are fine, which tells
// crypto/tls to use its default behaviour.
func RootCAs() *x509.CertPool {
	rootsOnce.Do(loadRoots)
	return rootsPool
}

// CertsLoaded reports how many certificates were read from disk, and from
// where. Surfaced on the health endpoint so a TLS failure is diagnosable
// without a debugger.
func CertsLoaded() (int, string) {
	rootsOnce.Do(loadRoots)
	return rootsLoaded, rootsSource
}

func loadRoots() {
	// On a normal desktop the system pool works; leave it alone.
	if system, err := x509.SystemCertPool(); err == nil && system != nil {
		if !isAndroid() {
			rootsSource = "system"
			return
		}
		rootsPool = system
	}
	if rootsPool == nil {
		rootsPool = x509.NewCertPool()
	}

	for _, dir := range androidCertDirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		added := 0
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			// Android stores one certificate per file, named by subject hash
			// (e.g. "a1b2c3d4.0"). Each is PEM followed by a human-readable
			// dump, which AppendCertsFromPEM ignores.
			pem, err := os.ReadFile(filepath.Join(dir, entry.Name()))
			if err != nil {
				continue
			}
			if rootsPool.AppendCertsFromPEM(pem) {
				added++
			}
		}
		if added > 0 {
			rootsLoaded += added
			if rootsSource == "" {
				rootsSource = dir
			} else {
				rootsSource += ", " + dir
			}
		}
	}

	if rootsLoaded == 0 {
		// Nothing found: fall back to the default rather than handing crypto/tls
		// an empty pool, which would reject every certificate outright.
		rootsPool = nil
		if rootsSource == "" {
			rootsSource = "none found"
		}
	}
}
