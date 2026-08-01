package server

import (
	"strings"
	"testing"

	"omnidrive/internal/provider"
)

// Credentials get copied straight out of a downloaded client_secret_*.json,
// quotes and all. A stray quote inside client_id makes Google reject the entire
// authorize request with a bare "400 ... malformed", which says nothing about
// the cause.
func TestSanitizeCredentialStripsJSONPasteArtefacts(t *testing.T) {
	const want = "108222675046-abc.apps.googleusercontent.com"
	cases := []string{
		want,
		`"108222675046-abc.apps.googleusercontent.com"`,
		`'108222675046-abc.apps.googleusercontent.com'`,
		`  "108222675046-abc.apps.googleusercontent.com",  `,
		"\n108222675046-abc.apps.googleusercontent.com\t",
		`"108222675046-abc.apps.googleusercontent.com",`,
	}
	for _, in := range cases {
		got, err := sanitizeCredential(in, "client ID")
		if err != nil {
			t.Errorf("sanitizeCredential(%q) errored: %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("sanitizeCredential(%q)\n got %q\nwant %q", in, got, want)
		}
	}
}

func TestSanitizeCredentialRejectsUnusableValues(t *testing.T) {
	for _, in := range []string{
		"has a space in it",
		"embedded\"quote.apps.googleusercontent.com",
		"tab\there",
		"new\nline",
	} {
		if _, err := sanitizeCredential(in, "client ID"); err == nil {
			t.Errorf("sanitizeCredential(%q) should have been rejected", in)
		}
	}
}

func TestSanitizeCredentialAllowsEmpty(t *testing.T) {
	// Empty is legitimate: it means "fall back to the stored or built-in one".
	got, err := sanitizeCredential("   ", "client secret")
	if err != nil || got != "" {
		t.Fatalf("got %q, %v; want empty and no error", got, err)
	}
}

func TestValidateClientIDCatchesWrongFieldPastes(t *testing.T) {
	// The secret pasted into the ID field is a very easy mistake: the two sit
	// next to each other in the console and in the JSON.
	err := validateClientID(provider.KindGoogleDrive, "GOCSPX-abcdefghijklmnop")
	if err == nil || !strings.Contains(err.Error(), "client secret") {
		t.Fatalf("secret-as-ID not caught, got %v", err)
	}

	if err := validateClientID(provider.KindGoogleDrive, "123456-abc"); err == nil {
		t.Fatal("a truncated Google client ID was accepted")
	}
	if err := validateClientID(provider.KindGoogleDrive, "123-abc.apps.googleusercontent.com"); err != nil {
		t.Fatalf("a valid Google client ID was rejected: %v", err)
	}

	if err := validateClientID(provider.KindOneDrive, "not-a-guid"); err == nil {
		t.Fatal("a non-GUID Microsoft client ID was accepted")
	}
	if err := validateClientID(provider.KindOneDrive, "00000000-1111-2222-3333-444444444444"); err != nil {
		t.Fatalf("a valid Microsoft GUID was rejected: %v", err)
	}

	// Dropbox app keys have no fixed shape, so nothing to assert against.
	if err := validateClientID(provider.KindDropbox, "abc123xyz"); err != nil {
		t.Fatalf("Dropbox key rejected: %v", err)
	}
	// Empty defers to the caller's fallback logic.
	if err := validateClientID(provider.KindGoogleDrive, ""); err != nil {
		t.Fatalf("empty client ID should defer, got %v", err)
	}
}
