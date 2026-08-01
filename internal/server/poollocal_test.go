package server

import (
	"net/http"
	"path/filepath"
	"testing"

	"omnidrive/internal/pool"
)

// Device storage must never join the combined cloud drive. Merging it would
// mean a file the user filed "in the cloud" could actually be sitting on the
// phone — and would vanish from the cloud when the SD card came out.
func TestPoolExcludesDeviceStorage(t *testing.T) {
	h := newHarness(t)

	// One cloud drive and one device folder.
	cloudID := h.connectDAV()
	var added map[string]any
	h.mustJSON(http.MethodPost, "/api/storage/add", map[string]any{
		"path": filepath.ToSlash(t.TempDir()), "label": "Internal storage",
	}, &added)

	var root struct {
		Files []struct {
			Name      string `json:"name"`
			AccountID string `json:"accountId"`
		} `json:"files"`
		Pooled map[string]any `json:"pooled"`
	}
	h.mustJSON(http.MethodGet, "/api/files", nil, &root)

	// The pool counts the cloud drive only.
	if got := int(root.Pooled["drives"].(float64)); got != 1 {
		t.Fatalf("combined drive spans %d drives, want 1 (the cloud one only)", got)
	}

	// Device storage still appears in its own right; the cloud account does
	// not, because it is represented by the combined entry.
	var sawLocal, sawPool, sawCloudDirectly bool
	for _, f := range root.Files {
		switch f.AccountID {
		case pool.ID:
			sawPool = true
		case cloudID:
			sawCloudDirectly = true
		default:
			sawLocal = true
		}
	}
	if !sawLocal {
		t.Error("device storage is missing from the file list")
	}
	if !sawPool {
		t.Error("the combined cloud drive is missing")
	}
	if sawCloudDirectly {
		t.Error("a cloud account is listed separately while pooling is on; it should be inside Cloud")
	}
}

// With no cloud accounts at all there is nothing to combine, so no empty
// "Cloud" entry should appear.
func TestNoPoolWithoutCloudDrives(t *testing.T) {
	h := newHarness(t)
	var added map[string]any
	h.mustJSON(http.MethodPost, "/api/storage/add", map[string]any{
		"path": filepath.ToSlash(t.TempDir()), "label": "Internal storage",
	}, &added)

	var root struct {
		Files []struct {
			AccountID string `json:"accountId"`
		} `json:"files"`
	}
	h.mustJSON(http.MethodGet, "/api/files", nil, &root)
	for _, f := range root.Files {
		if f.AccountID == pool.ID {
			t.Fatal("a combined cloud drive was offered with no cloud drives connected")
		}
	}
}

// Turning pooling off restores the individual cloud accounts to the list.
func TestDisablingPoolRestoresIndividualCloudDrives(t *testing.T) {
	h := newHarness(t)
	cloudID := h.connectDAV()

	h.mustJSON(http.MethodPut, "/api/settings", map[string]any{"poolEnabled": false}, nil)

	var root struct {
		Files []struct {
			AccountID string `json:"accountId"`
		} `json:"files"`
	}
	h.mustJSON(http.MethodGet, "/api/files", nil, &root)

	var sawCloud, sawPool bool
	for _, f := range root.Files {
		if f.AccountID == cloudID {
			sawCloud = true
		}
		if f.AccountID == pool.ID {
			sawPool = true
		}
	}
	if !sawCloud {
		t.Error("the cloud account is not listed with pooling off")
	}
	if sawPool {
		t.Error("the combined drive is still listed with pooling off")
	}
}
