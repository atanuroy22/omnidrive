package portable

import (
	"bytes"
	"errors"
	"testing"

	"omnidrive/internal/provider"
	"omnidrive/internal/store"
)

func testStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddAccount(provider.KindWebDAV, "Test",
		provider.Credentials{"url": "https://example.com/dav", "username": "u", "password": "secret-token"}); err != nil {
		t.Fatal(err)
	}
	return st
}

// Export and restore with no passphrase at all: one tap either way.
func TestUnprotectedBundleRoundTrips(t *testing.T) {
	src := testStore(t)

	blob, err := Export(src, "")
	if err != nil {
		t.Fatalf("export without a passphrase: %v", err)
	}
	// Unprotected does not mean unencrypted: credentials must not sit in the
	// file in plain sight.
	if bytes.Contains(blob, []byte("secret-token")) {
		t.Fatal("credentials appear verbatim in the bundle")
	}

	dst, err := store.Open(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	summary, added, _, err := Import(dst, blob, "", false)
	if err != nil {
		t.Fatalf("import without a passphrase: %v", err)
	}
	if added != 1 || len(summary.Accounts) != 1 {
		t.Fatalf("added %d accounts, summary %v", added, summary.Accounts)
	}
	if got := dst.Accounts()[0].Creds["password"]; got != "secret-token" {
		t.Fatalf("credentials did not survive the round trip: %q", got)
	}
}

// A passphrase still works for anyone who wants one.
func TestProtectedBundleStillWorks(t *testing.T) {
	src := testStore(t)

	blob, err := Export(src, "a real passphrase")
	if err != nil {
		t.Fatal(err)
	}
	dst, err := store.Open(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := Import(dst, blob, "a real passphrase", false); err != nil {
		t.Fatalf("protected round trip failed: %v", err)
	}
}

// Opening a protected bundle with no passphrase must say so, not report the
// file as corrupt — the UI uses this to know when to ask.
func TestProtectedBundleReportsItself(t *testing.T) {
	src := testStore(t)
	blob, err := Export(src, "a real passphrase")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Decode(blob, ""); !errors.Is(err, ErrPassphraseRequired) {
		t.Fatalf("got %v, want ErrPassphraseRequired", err)
	}
}

// A short passphrase is a typo, not a choice; blank is the way to opt out.
func TestShortPassphraseRejected(t *testing.T) {
	src := testStore(t)
	if _, err := Export(src, "abc"); err == nil {
		t.Fatal("a three-character passphrase was accepted")
	}
	if _, err := Export(src, ""); err != nil {
		t.Fatalf("blank should mean unprotected, got %v", err)
	}
}
