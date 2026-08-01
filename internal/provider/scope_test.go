package provider

import (
	"strings"
	"testing"
)

// The narrow scope is the whole point: a project using only non-sensitive
// scopes can be published without review, so its tokens never expire.
func TestScopedEndpointsNarrowsGoogle(t *testing.T) {
	full, _ := ScopedEndpoints(KindGoogleDrive, ScopeFull)
	if !contains(full.Scopes, "https://www.googleapis.com/auth/drive") {
		t.Fatalf("full mode lost the drive scope: %v", full.Scopes)
	}
	narrow, _ := ScopedEndpoints(KindGoogleDrive, ScopeAppFiles)
	for _, s := range narrow.Scopes {
		if s == "https://www.googleapis.com/auth/drive" {
			t.Fatalf("narrow mode still requests the restricted scope: %v", narrow.Scopes)
		}
		if !strings.HasPrefix(s, "https://www.googleapis.com/auth/") {
			t.Errorf("unexpected scope %q", s)
		}
	}
	if !contains(narrow.Scopes, "https://www.googleapis.com/auth/drive.file") {
		t.Fatalf("narrow mode missing drive.file: %v", narrow.Scopes)
	}
	// Other providers must be unaffected by the mode.
	a, _ := ScopedEndpoints(KindDropbox, ScopeAppFiles)
	b, _ := ScopedEndpoints(KindDropbox, ScopeFull)
	if strings.Join(a.Scopes, ",") != strings.Join(b.Scopes, ",") {
		t.Error("dropbox scopes changed with mode")
	}
}

func TestScopeChoicesDefaultIsTheSafeOne(t *testing.T) {
	choices := ScopeChoices(KindGoogleDrive)
	if len(choices) != 2 {
		t.Fatalf("want 2 choices, got %d", len(choices))
	}
	var def ScopeChoice
	for _, c := range choices {
		if c.Default {
			def = c
		}
	}
	if def.Mode != ScopeAppFiles {
		t.Fatalf("default should avoid the restricted scope, got %q", def.Mode)
	}
	if ScopeChoices(KindS3) != nil {
		t.Error("non-OAuth provider offered scope choices")
	}
}

func contains(s []string, want string) bool {
	for _, v := range s {
		if v == want {
			return true
		}
	}
	return false
}
