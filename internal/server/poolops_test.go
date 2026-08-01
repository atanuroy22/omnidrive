package server

import (
	"net/http"
	"testing"

	"omnidrive/internal/pool"
)

// A folder shown once in the combined view must disappear once. It physically
// exists on several drives, and is addressed by path rather than by any one
// account — so the ordinary delete path, which needs a real account, could not
// touch it at all.
func TestPoolFolderDeleteRemovesItEverywhere(t *testing.T) {
	ph := newPoolHarness(t, map[string][]string{
		"alpha": {"Shared/a.txt", "Solo/only.txt"},
		"bravo": {"Shared/b.txt"},
	})

	// Present on both drives before.
	if !ph.drives[0].has("Shared/") || !ph.drives[1].has("Shared/") {
		t.Fatal("setup: Shared is not on both drives")
	}

	var res struct {
		Results []struct {
			OK    bool   `json:"ok"`
			Error string `json:"error"`
		} `json:"results"`
	}
	ph.mustJSON(http.MethodPost, "/api/files/delete", map[string]any{
		"account": pool.ID, "ids": []string{"Shared"},
	}, &res)

	if len(res.Results) != 1 || !res.Results[0].OK {
		t.Fatalf("delete reported %+v", res.Results)
	}
	for i, d := range ph.drives {
		if d.has("Shared/") {
			t.Errorf("Shared still present on drive %d", i)
		}
	}
	// An unrelated folder must be untouched.
	if !ph.drives[0].has("Solo/") {
		t.Error("delete removed an unrelated folder")
	}
	// And it is gone from the combined listing.
	for _, f := range ph.listPool(t, "") {
		if f.Name == "Shared" {
			t.Error("Shared still appears in the combined view")
		}
	}
}

// Renaming the single entry the user sees has to rename it on every drive, or
// the folder would split into two.
func TestPoolFolderRenameAppliesEverywhere(t *testing.T) {
	ph := newPoolHarness(t, map[string][]string{
		"alpha": {"Photos/a.jpg"},
		"bravo": {"Photos/b.jpg"},
	})

	ph.mustJSON(http.MethodPost, "/api/files/rename", map[string]any{
		"account": pool.ID, "id": "Photos", "name": "Pictures",
	}, nil)

	for i, d := range ph.drives {
		if d.has("Photos/") {
			t.Errorf("drive %d still has the old folder name", i)
		}
		if !d.has("Pictures/") {
			t.Errorf("drive %d was not renamed", i)
		}
	}

	// One entry, with both drives' contents still under it.
	var seen int
	for _, f := range ph.listPool(t, "") {
		if f.Name == "Pictures" {
			seen++
		}
	}
	if seen != 1 {
		t.Fatalf("the renamed folder appears %d times, want 1", seen)
	}
	if got := len(ph.listPool(t, "Pictures")); got != 2 {
		t.Fatalf("renamed folder holds %d files, want 2", got)
	}
}

// Deleting pooled *files* still addresses their real drive, which must keep
// working alongside the folder case.
func TestPoolFileDeleteUsesItsOwnDrive(t *testing.T) {
	ph := newPoolHarness(t, map[string][]string{
		"alpha": {"Shared/a.txt"},
		"bravo": {"Shared/b.txt"},
	})

	var target listedFile
	for _, f := range ph.listPool(t, "Shared") {
		if f.Name == "a.txt" {
			target = f
		}
	}
	if target.ID == "" {
		t.Fatal("could not find the file to delete")
	}

	// Pooled files carry their real account, so this is an ordinary delete.
	var accounts []map[string]any
	ph.mustJSON(http.MethodGet, "/api/accounts", nil, &accounts)

	var res map[string]any
	for _, a := range accounts {
		ph.mustJSON(http.MethodPost, "/api/files/delete", map[string]any{
			"account": a["id"], "ids": []string{target.ID},
		}, &res)
	}

	remaining := ph.listPool(t, "Shared")
	for _, f := range remaining {
		if f.Name == "a.txt" {
			t.Fatal("the file is still listed after deletion")
		}
	}
	if len(remaining) != 1 {
		t.Fatalf("%d files left, want 1 (b.txt)", len(remaining))
	}
}
