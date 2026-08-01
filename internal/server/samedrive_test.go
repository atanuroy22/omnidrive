package server

import (
	"net/http"
	"testing"
)

// Moving a file from one folder to another on the same drive is the commonest
// operation a file manager performs. Excluding the source drive from the
// destination list made it impossible.
func TestTransferTargetsIncludeTheSourceDrive(t *testing.T) {
	h := newHarness(t)
	davID := h.connectDAV()

	var targets []map[string]any
	h.mustJSON(http.MethodGet, "/api/transfer/targets?exclude="+davID, nil, &targets)

	// Pooling is on by default, so the WebDAV account is represented by the
	// combined entry rather than itself — but *something* must be offered.
	if len(targets) == 0 {
		t.Fatal("no destinations offered at all")
	}

	// With pooling off the account itself must be offered, source or not.
	h.mustJSON(http.MethodPut, "/api/settings", map[string]any{"poolEnabled": false}, nil)
	h.mustJSON(http.MethodGet, "/api/transfer/targets?exclude="+davID, nil, &targets)

	var sawSource bool
	for _, tgt := range targets {
		if tgt["id"] == davID {
			sawSource = true
		}
	}
	if !sawSource {
		t.Fatal("the source drive is not offered as a destination, so folder-to-folder " +
			"copying on one drive is impossible")
	}
}

// A same-drive copy has to actually work, not merely be offered.
func TestSameDriveFolderToFolderCopy(t *testing.T) {
	h := newHarness(t)
	h.dav.mu.Lock()
	h.dav.files["From/"] = nil
	h.dav.files["From/note.txt"] = []byte("same drive payload")
	h.dav.files["To/"] = nil
	h.dav.mu.Unlock()

	acc := h.connectDAV()

	var res map[string]any
	h.mustJSON(http.MethodPost, "/api/transfer", map[string]any{
		"fromAccount": acc, "toAccount": acc, "toFolder": "To",
		"ids": []string{"From/note.txt"}, "move": false,
	}, &res)

	deadline := 60
	for i := 0; i < deadline; i++ {
		if h.dav.has("To/note.txt") {
			return
		}
		sleepMillis(100)
	}
	t.Fatal("the file never arrived in the destination folder on the same drive")
}

// Copying a folder into itself would recurse forever, so it must be refused
// rather than attempted.
func TestRefusesCopyingAFolderIntoItself(t *testing.T) {
	h := newHarness(t)
	h.dav.mu.Lock()
	h.dav.files["Deep/"] = nil
	h.dav.files["Deep/Inner/"] = nil
	h.dav.mu.Unlock()
	acc := h.connectDAV()

	for _, dest := range []string{"Deep", "Deep/Inner"} {
		resp, body := h.do(http.MethodPost, "/api/transfer", map[string]any{
			"fromAccount": acc, "toAccount": acc, "toFolder": dest,
			"ids": []string{"Deep"}, "move": false,
		})
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("copying Deep into %q returned HTTP %d, want 400: %s",
				dest, resp.StatusCode, body)
		}
	}
}

func TestWithinTarget(t *testing.T) {
	cases := []struct {
		dest, item string
		want       bool
	}{
		{"Photos", "Photos", true},          // the folder itself
		{"Photos/2024", "Photos", true},     // a descendant
		{"Photos/2024/Raw", "Photos", true}, // deeper still
		{"PhotosOld", "Photos", false},      // merely a name prefix
		{"Other", "Photos", false},          // unrelated
		{"", "Photos", false},               // the drive root
		{"Photos", "", false},               // nothing selected
		{"/Photos/", "Photos", true},        // slashes normalised
		{`Photos\2024`, "Photos", true},     // backslashes too
	}
	for _, tc := range cases {
		if got := withinTarget(tc.dest, tc.item); got != tc.want {
			t.Errorf("withinTarget(%q, %q) = %v, want %v", tc.dest, tc.item, got, tc.want)
		}
	}
}
