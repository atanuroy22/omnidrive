// Package androidnet hardens Go's networking stack for Android userspace.
//
// A CGO-free Go binary uses the pure-Go DNS resolver, which reads
// /etc/resolv.conf. Android has no such file, so the resolver falls back to
// 127.0.0.1:53 and every lookup fails with "server misbehaving" — the single
// most common reason a Linux/arm64 Go binary appears to have no internet on a
// phone. We detect that situation and install a resolver that talks to the
// nameservers Android actually uses.
package androidnet

import (
	"context"
	"net"
	"os"
	"strings"
	"sync/atomic"
	"time"
)

// EnvDNS lets the host application supply the nameservers it obtained from a
// platform API. The Android app fills this in from ConnectivityManager.
const EnvDNS = "OMNIDRIVE_DNS"

// Public fallbacks, used only when the system nameservers cannot be read.
var fallbackDNS = []string{
	"1.1.1.1:53",
	"8.8.8.8:53",
	"9.9.9.9:53",
	"[2606:4700:4700::1111]:53",
}

var active atomic.Bool

// Patched reports whether the custom resolver was installed.
func Patched() bool { return active.Load() }

// Servers lists the nameservers currently in rotation, for diagnostics.
var servers []string

func Servers() []string { return append([]string(nil), servers...) }

// Install replaces net.DefaultResolver when the host looks like Android and
// /etc/resolv.conf is unusable. It is a no-op on desktop Linux, macOS and
// Windows, so the same code path is safe everywhere.
func Install() {
	if !needsPatch() {
		return
	}
	servers = discoverServers()
	if len(servers) == 0 {
		servers = fallbackDNS
	}

	var rr uint32
	dial := func(ctx context.Context, network, _ string) (net.Conn, error) {
		d := net.Dialer{Timeout: 5 * time.Second}
		n := atomic.AddUint32(&rr, 1)
		var firstErr error
		// Try every server once, starting at a rotating offset, so a single
		// dead nameserver cannot wedge the whole process.
		for i := range servers {
			addr := servers[(int(n)+i)%len(servers)]
			// UDP first; the resolver retries over TCP itself when truncated.
			conn, err := d.DialContext(ctx, network, addr)
			if err == nil {
				return conn, nil
			}
			if firstErr == nil {
				firstErr = err
			}
		}
		return nil, firstErr
	}

	net.DefaultResolver = &net.Resolver{PreferGo: true, Dial: dial}
	active.Store(true)
}

// needsPatch is true when we are on Android-like userspace and the stock
// resolver has nothing to work with.
func needsPatch() bool {
	if !isAndroid() {
		return false
	}
	b, err := os.ReadFile("/etc/resolv.conf")
	if err != nil {
		return true
	}
	// A resolv.conf with no nameserver line is just as useless as a missing one.
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "nameserver ") {
			return false
		}
	}
	return true
}

func isAndroid() bool {
	if os.Getenv("ANDROID_ROOT") != "" || os.Getenv("ANDROID_DATA") != "" {
		return true
	}
	if os.Getenv("TERMUX_VERSION") != "" || strings.Contains(os.Getenv("PREFIX"), "com.termux") {
		return true
	}
	if _, err := os.Stat("/system/build.prop"); err == nil {
		return true
	}
	return false
}

// discoverServers collects nameservers to try, preferring the ones the current
// network actually handed out over hardcoded public ones.
//
// This deliberately does not shell out to `getprop`. Doing so costs a process
// spawn, and on Android it is fatal: os/exec.LookPath probes candidates with
// faccessat2(2), which Android's seccomp-bpf policy answers with SIGSYS rather
// than ENOSYS — killing the process before main() gets going. The host
// application passes the servers in through EnvDNS instead, which the Android
// app reads from ConnectivityManager and the Termux launcher script fills in
// from getprop (a shell may call it safely; a Go binary may not).
func discoverServers() []string {
	var out []string
	seen := map[string]bool{}
	add := func(ip string) {
		ip = strings.TrimSpace(ip)
		// Link-local IPv6 arrives scoped, e.g. "fe80::1%wlan0"; the zone is
		// meaningless to us and breaks ParseIP.
		if i := strings.IndexByte(ip, '%'); i >= 0 {
			ip = ip[:i]
		}
		if ip == "" || seen[ip] {
			return
		}
		parsed := net.ParseIP(ip)
		if parsed == nil || parsed.IsUnspecified() || parsed.IsLoopback() {
			return
		}
		seen[ip] = true
		out = append(out, net.JoinHostPort(ip, "53"))
	}

	for _, ip := range strings.FieldsFunc(os.Getenv(EnvDNS), func(r rune) bool {
		return r == ',' || r == ' ' || r == ';'
	}) {
		add(ip)
	}

	// Public resolvers always come last, so a network that blocks them still
	// works via its own servers, and a stale operator address cannot wedge us
	// after switching between Wi-Fi and mobile data.
	return append(out, fallbackDNS...)
}
