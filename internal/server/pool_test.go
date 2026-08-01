package server

import (
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"omnidrive/internal/pool"
)

// poolHarness sets up several *cloud* drives with overlapping folder
// structures, which is the situation the combined view exists to hide.
//
// Cloud specifically: device storage is deliberately excluded from the pool,
// so building this out of local folders would test nothing.
type poolHarness struct {
	*harness
	drives []*davFS
}

func newPoolHarness(t *testing.T, layout map[string][]string) *poolHarness {
	t.Helper()
	h := newHarness(t)
	ph := &poolHarness{harness: h}

	names := make([]string, 0, len(layout))
	for name := range layout {
		names = append(names, name)
	}
	sort.Strings(names) // deterministic order, so allocation is reproducible

	for _, name := range names {
		dav := newDavFSFrom(layout[name])
		davSrv := httptest.NewServer(dav)
		t.Cleanup(davSrv.Close)
		ph.drives = append(ph.drives, dav)

		var out map[string]any
		h.mustJSON(http.MethodPost, "/api/connect/direct", map[string]any{
			"kind": "webdav", "label": name,
			"fields": map[string]string{
				"url": davSrv.URL + "/dav/", "username": "user", "password": "pass",
			},
		}, &out)
		if out["id"] == nil {
			t.Fatalf("could not connect drive %q: %v", name, out)
		}
	}
	return ph
}

// has reports whether a drive holds a path.
func (d *davFS) has(path string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, ok := d.files[path]
	return ok
}

func (ph *poolHarness) listPool(t *testing.T, folder string) []listedFile {
	t.Helper()
	var res struct {
		Files []listedFile `json:"files"`
	}
	ph.mustJSON(http.MethodGet, "/api/files?account="+pool.ID+"&folder="+folder, nil, &res)
	return res.Files
}

// The core promise: one namespace, with folders that exist on several drives
// appearing exactly once.
func TestPoolMergesFoldersAcrossDrives(t *testing.T) {
	ph := newPoolHarness(t, map[string][]string{
		"alpha": {"Photos/one.jpg", "Docs/a.txt"},
		"bravo": {"Photos/two.jpg", "Music/song.mp3"},
	})

	root := ph.listPool(t, "")
	counts := map[string]int{}
	for _, f := range root {
		counts[f.Name]++
	}
	for _, name := range []string{"Photos", "Docs", "Music"} {
		if counts[name] != 1 {
			t.Errorf("folder %q appears %d times in the combined view, want exactly 1", name, counts[name])
		}
	}

	// Photos lives on both drives; its contents must come from both.
	photos := ph.listPool(t, "Photos")
	names := map[string]bool{}
	for _, f := range photos {
		names[f.Name] = true
	}
	if !names["one.jpg"] || !names["two.jpg"] {
		t.Fatalf("merged Photos is missing files from one of the drives: %+v", photos)
	}
}

// Capacity is presented as one number, the sum of the parts.
func TestPoolQuotaIsTheSumOfDrives(t *testing.T) {
	ph := newPoolHarness(t, map[string][]string{"alpha": {"a.txt"}, "bravo": {"b.txt"}})

	var accounts []map[string]any
	ph.mustJSON(http.MethodGet, "/api/accounts", nil, &accounts)

	var wantUsed, wantTotal float64
	for _, a := range accounts {
		wantUsed += a["quotaUsed"].(float64)
		wantTotal += a["quotaTotal"].(float64)
	}

	var root struct {
		Pooled map[string]any `json:"pooled"`
	}
	ph.mustJSON(http.MethodGet, "/api/files", nil, &root)
	if root.Pooled == nil {
		t.Fatal("root listing carried no combined-drive figures")
	}
	if got := root.Pooled["quotaTotal"].(float64); got != wantTotal {
		t.Errorf("combined total = %v, want %v (the sum of all drives)", got, wantTotal)
	}
	if got := root.Pooled["quotaUsed"].(float64); got != wantUsed {
		t.Errorf("combined used = %v, want %v", got, wantUsed)
	}
	if got := root.Pooled["drives"].(float64); int(got) != len(accounts) {
		t.Errorf("combined drive count = %v, want %d", got, len(accounts))
	}
}

// Uploading names a path, never a drive; the allocator decides where it goes,
// and the file must then be visible through the pool regardless.
func TestPoolUploadPicksADriveAndStaysVisible(t *testing.T) {
	ph := newPoolHarness(t, map[string][]string{
		"alpha": {"Photos/one.jpg"},
		"bravo": {"Photos/two.jpg"},
	})

	body := strings.NewReader("a pooled payload")
	req, _ := http.NewRequest(http.MethodPost,
		ph.ts.URL+"/api/upload?account="+pool.ID+"&folder=Photos&name=new.txt&size=16", body)
	resp, err := ph.ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("pooled upload: HTTP %d", resp.StatusCode)
	}

	var found bool
	for _, f := range ph.listPool(t, "Photos") {
		if f.Name == "new.txt" {
			found = true
		}
	}
	if !found {
		t.Fatal("a file uploaded through the pool is not visible in the pool")
	}

	// And it exists on exactly one drive: the pool must not duplicate data.
	copies := 0
	for _, d := range ph.drives {
		if d.has("Photos/new.txt") {
			copies++
		}
	}
	if copies != 1 {
		t.Fatalf("the file exists on %d drives, want exactly 1", copies)
	}
}

// A folder created in the pool must be usable immediately, including on a
// drive that had no such path before.
func TestPoolMkdirThenUpload(t *testing.T) {
	ph := newPoolHarness(t, map[string][]string{"alpha": {"x.txt"}, "bravo": {"y.txt"}})

	ph.mustJSON(http.MethodPost, "/api/files/mkdir", map[string]any{
		"account": pool.ID, "parent": "", "name": "Fresh",
	}, nil)

	body := strings.NewReader("deep")
	req, _ := http.NewRequest(http.MethodPost,
		ph.ts.URL+"/api/upload?account="+pool.ID+"&folder=Fresh/Nested&name=deep.txt&size=4", body)
	resp, err := ph.ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("upload into a new nested pool folder: HTTP %d", resp.StatusCode)
	}

	var found bool
	for _, f := range ph.listPool(t, "Fresh/Nested") {
		if f.Name == "deep.txt" {
			found = true
		}
	}
	if !found {
		t.Fatal("intermediate folders were not created on the receiving drive")
	}
}

// Turning the toggle off must restore the plain per-drive listing.
func TestPoolCanBeDisabled(t *testing.T) {
	ph := newPoolHarness(t, map[string][]string{"alpha": {"a.txt"}})

	hasPool := func() bool {
		var res struct {
			Files []struct {
				AccountID string `json:"accountId"`
			} `json:"files"`
		}
		ph.mustJSON(http.MethodGet, "/api/files", nil, &res)
		for _, f := range res.Files {
			if f.AccountID == pool.ID {
				return true
			}
		}
		return false
	}

	if !hasPool() {
		t.Fatal("the combined drive should be on by default")
	}
	ph.mustJSON(http.MethodPut, "/api/settings", map[string]any{"poolEnabled": false}, nil)
	if hasPool() {
		t.Fatal("the combined drive still appears after being switched off")
	}
	ph.mustJSON(http.MethodPut, "/api/settings", map[string]any{"poolEnabled": true}, nil)
	if !hasPool() {
		t.Fatal("the combined drive did not come back")
	}
}

func TestPoolCleanPath(t *testing.T) {
	cases := map[string]string{
		"":                "",
		"/":               "",
		"Photos":          "Photos",
		"/Photos/":        "Photos",
		"Photos//2024":    "Photos/2024",
		`Photos\2024`:     "Photos/2024",
		"Photos/../Music": "Music",
		"../../etc":       "etc",
	}
	for in, want := range cases {
		if got := pool.CleanPath(in); got != want {
			t.Errorf("CleanPath(%q) = %q, want %q", in, got, want)
		}
	}
}
