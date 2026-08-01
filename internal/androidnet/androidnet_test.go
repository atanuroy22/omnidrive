package androidnet

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestDiscoverServersFromEnv(t *testing.T) {
	t.Setenv(EnvDNS, "192.168.1.1, 2001:4860:4860::8888 ;10.0.0.1")

	got := discoverServers()
	want := []string{"192.168.1.1:53", "[2001:4860:4860::8888]:53", "10.0.0.1:53"}
	for i, w := range want {
		if i >= len(got) || got[i] != w {
			t.Fatalf("server %d = %q, want %q (all: %v)", i, safeIndex(got, i), w, got)
		}
	}
	// Public resolvers must come last so a network that blocks them still
	// works through its own servers.
	if !strings.HasPrefix(got[len(want)], "1.1.1.1") {
		t.Errorf("fallbacks should follow the supplied servers, got %v", got)
	}
}

func TestDiscoverServersRejectsUnusableAddresses(t *testing.T) {
	t.Setenv(EnvDNS, "127.0.0.1,0.0.0.0,not-an-ip,,8.8.4.4,8.8.4.4")

	got := discoverServers()
	seen := map[string]int{}
	for _, s := range got {
		seen[s]++
	}
	if seen["127.0.0.1:53"] != 0 {
		t.Error("loopback accepted: that is the broken default we exist to replace")
	}
	if seen["0.0.0.0:53"] != 0 {
		t.Error("unspecified address accepted")
	}
	if n := seen["8.8.4.4:53"]; n != 1 {
		t.Errorf("8.8.4.4 appears %d times, want exactly 1 (duplicates not collapsed)", n)
	}
}

// Link-local addresses arrive from Android's ConnectivityManager with a zone
// suffix that net.ParseIP rejects.
func TestDiscoverServersStripsIPv6Zone(t *testing.T) {
	t.Setenv(EnvDNS, "fe80::1%wlan0")

	got := discoverServers()
	if got[0] != "[fe80::1]:53" {
		t.Fatalf("got %q, want %q", got[0], "[fe80::1]:53")
	}
}

func TestDiscoverServersAlwaysHasFallbacks(t *testing.T) {
	t.Setenv(EnvDNS, "")
	if got := discoverServers(); len(got) < len(fallbackDNS) {
		t.Fatalf("no usable servers with an empty environment: %v", got)
	}
}

// This is the regression guard for a crash seen on a real device: Android's
// seccomp-bpf policy answers faccessat2(2) with SIGSYS instead of ENOSYS, so
// any call into os/exec.LookPath kills the process before main() can run.
// Keeping os/exec out of the dependency graph entirely is the only reliable
// defence — a SIGSYS cannot be recovered from.
func TestServerBinaryDoesNotLinkOsExec(t *testing.T) {
	// Specifically the linux build: that is what ships to Android, both in the
	// APK and under Termux. Desktop builds may legitimately use os/exec — the
	// browser launcher does — which is why those variants are behind build
	// tags that exclude linux.
	cmd := exec.Command("go", "list", "-deps", "../../cmd/omnidrive")
	cmd.Env = append(os.Environ(), "GOOS=linux", "GOARCH=arm64", "CGO_ENABLED=0")

	out, err := cmd.Output()
	if err != nil {
		t.Skipf("go list unavailable: %v", err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.TrimSpace(line) == "os/exec" {
			t.Fatal("os/exec is linked into the linux/Android build.\n" +
				"On Android this is fatal: LookPath probes with faccessat2(2), " +
				"which seccomp turns into SIGSYS and kills the process at startup.\n" +
				"Put the platform-specific code behind a build tag that excludes linux.")
		}
	}
}

func safeIndex(s []string, i int) string {
	if i < len(s) {
		return s[i]
	}
	return "<missing>"
}
