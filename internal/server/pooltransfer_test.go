package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"omnidrive/internal/pool"
)

// With combining on, copy/move must offer the combined drive — not the
// accounts hidden inside it. Offering them individually while the file list
// shows one drive was inconsistent.
func TestTransferTargetsFollowPooling(t *testing.T) {
	h := newHarness(t)
	cloudID := h.connectDAV()

	targets := func() []map[string]any {
		var out []map[string]any
		h.mustJSON(http.MethodGet, "/api/transfer/targets", nil, &out)
		return out
	}

	var sawPool, sawCloud bool
	for _, tgt := range targets() {
		if tgt["id"] == pool.ID {
			sawPool = true
		}
		if tgt["id"] == cloudID {
			sawCloud = true
		}
	}
	if !sawPool {
		t.Error("the combined drive is not offered as a destination")
	}
	if sawCloud {
		t.Error("an individual cloud account is offered while combining is on")
	}

	// Off: the accounts come back as destinations, and the pool goes away.
	h.mustJSON(http.MethodPut, "/api/settings", map[string]any{"poolEnabled": false}, nil)
	sawPool, sawCloud = false, false
	for _, tgt := range targets() {
		if tgt["id"] == pool.ID {
			sawPool = true
		}
		if tgt["id"] == cloudID {
			sawCloud = true
		}
	}
	if sawPool {
		t.Error("the combined drive is still offered with combining off")
	}
	if !sawCloud {
		t.Error("the cloud account is not offered with combining off")
	}
}

// Copying into the combined drive places files by allocation rather than
// requiring a drive to be named.
func TestTransferIntoPoolPlacesFiles(t *testing.T) {
	ph := newPoolHarness(t, map[string][]string{
		"alpha": {"Existing/a.txt"},
		"bravo": {"Existing/b.txt"},
	})

	// A device folder to copy *from*.
	src := newDavFSFrom([]string{"Source/one.txt", "Source/two.txt"})
	srcSrv := newTestDav(t, src)
	var acc map[string]any
	ph.mustJSON(http.MethodPost, "/api/connect/direct", map[string]any{
		"kind": "webdav", "label": "source",
		"fields": map[string]string{"url": srcSrv + "/dav/", "username": "user", "password": "pass"},
	}, &acc)
	srcID, _ := acc["id"].(string)

	var res map[string]any
	ph.mustJSON(http.MethodPost, "/api/transfer", map[string]any{
		"fromAccount": srcID,
		"toAccount":   pool.ID,
		"toFolder":    "Landed",
		"ids":         []string{"Source/one.txt"},
		"move":        false,
	}, &res)
	if res["to"] != pool.Label {
		t.Fatalf("transfer reported destination %v, want %q", res["to"], pool.Label)
	}

	// The transfer runs in the background; wait for it to appear in the pool.
	if !eventuallyInPool(t, ph, "Landed", "one.txt") {
		t.Fatal("the copied file never appeared in the combined drive")
	}
}

func eventuallyInPool(t *testing.T, ph *poolHarness, folder, name string) bool {
	t.Helper()
	for i := 0; i < 60; i++ {
		for _, f := range ph.listPool(t, folder) {
			if f.Name == name {
				return true
			}
		}
		sleepMillis(100)
	}
	return false
}

// newTestDav starts a throwaway WebDAV server and returns its base URL.
func newTestDav(t *testing.T, fs *davFS) string {
	t.Helper()
	srv := httptest.NewServer(fs)
	t.Cleanup(srv.Close)
	return srv.URL
}

func sleepMillis(n int) { time.Sleep(time.Duration(n) * time.Millisecond) }
