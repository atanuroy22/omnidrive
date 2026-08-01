package server

import (
	"path/filepath"
	"testing"
)

// A folder inside a volume already offered must not be offered again: it would
// present the same files under a second name, and auto-add would connect both.
// That is how "Internal storage" and "Home" both appeared on a phone.
func TestIsWithin(t *testing.T) {
	cases := []struct {
		path, root string
		want       bool
	}{
		{filepath.FromSlash("/storage/emulated/0/Download"), filepath.FromSlash("/storage/emulated/0"), true},
		{filepath.FromSlash("/storage/emulated/0"), filepath.FromSlash("/storage/emulated/0"), true},
		{filepath.FromSlash("/data/user/0/com.omnidrive.app/files"), filepath.FromSlash("/storage/emulated/0"), false},
		{filepath.FromSlash("/storage/1A2B-3C4D"), filepath.FromSlash("/storage/emulated/0"), false},
		// A sibling whose name merely starts with the root's name.
		{filepath.FromSlash("/storage/emulated/00"), filepath.FromSlash("/storage/emulated/0"), false},
		{filepath.FromSlash("/"), filepath.FromSlash("/storage"), false},
	}
	for _, tc := range cases {
		if got := isWithin(tc.path, tc.root); got != tc.want {
			t.Errorf("isWithin(%q, %q) = %v, want %v", tc.path, tc.root, got, tc.want)
		}
	}
}

// Whatever the platform, discovery must never offer the same folder twice, and
// auto-add must have exactly one obvious target.
func TestDiscoverVolumesHasNoOverlap(t *testing.T) {
	volumes := discoverVolumes()

	seen := map[string]string{}
	for _, v := range volumes {
		if prev, dup := seen[v.Path]; dup {
			t.Errorf("path %q listed twice (as %q and %q)", v.Path, prev, v.Label)
		}
		seen[v.Path] = v.Label
	}

	for i, a := range volumes {
		for j, b := range volumes {
			if i == j {
				continue
			}
			if isWithin(a.Path, b.Path) {
				t.Errorf("%q (%s) sits inside %q (%s); it should not be listed separately",
					a.Label, a.Path, b.Label, b.Path)
			}
		}
	}

	primaries := 0
	for _, v := range volumes {
		if v.Primary {
			primaries++
		}
	}
	if primaries > 1 {
		t.Errorf("%d volumes marked primary; auto-add would connect several drives", primaries)
	}
}
