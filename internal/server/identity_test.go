package server

import (
	"net/http"
	"testing"

	"omnidrive/internal/provider"
)

func TestIdentityKeyIgnoresVolatileFields(t *testing.T) {
	// Two sign-ins of the same Google account: different tokens, same storage.
	a := provider.Credentials{
		provider.CredAccountName: "atanu@example.com",
		provider.CredAccessToken: "token-one", provider.CredRefreshToken: "refresh-one",
	}
	b := provider.Credentials{
		provider.CredAccountName: "ATANU@example.com  ",
		provider.CredAccessToken: "token-two", provider.CredRefreshToken: "refresh-two",
	}
	if identityKey(provider.KindGoogleDrive, a) != identityKey(provider.KindGoogleDrive, b) {
		t.Fatal("the same account signed in twice produced different identities")
	}

	other := provider.Credentials{provider.CredAccountName: "someone.else@example.com"}
	if identityKey(provider.KindGoogleDrive, a) == identityKey(provider.KindGoogleDrive, other) {
		t.Fatal("different accounts collapsed to one identity")
	}
	// Same email on a different provider is a different drive.
	if identityKey(provider.KindGoogleDrive, a) == identityKey(provider.KindDropbox, a) {
		t.Fatal("identity ignored the provider")
	}
}

func TestIdentityKeyForCredentialProviders(t *testing.T) {
	dav := func(url, user string) string {
		return identityKey(provider.KindWebDAV, provider.Credentials{
			"url": url, "username": user, "password": "irrelevant",
		})
	}
	if dav("https://x/dav/", "me") != dav("https://x/dav", "ME") {
		t.Error("trailing slash or case changed the WebDAV identity")
	}
	if dav("https://x/dav", "me") == dav("https://y/dav", "me") {
		t.Error("different WebDAV servers collapsed")
	}

	s3 := func(bucket, prefix string) string {
		return identityKey(provider.KindS3, provider.Credentials{
			"endpoint": "https://s3.example.com", "bucket": bucket,
			"prefix": prefix, "secretKey": "changes",
		})
	}
	if s3("b", "p/") != s3("b", "/p") {
		t.Error("prefix slashes changed the S3 identity")
	}
	if s3("b", "p") == s3("other", "p") {
		t.Error("different buckets collapsed")
	}

	local := func(root string) string {
		return identityKey(provider.KindLocal, provider.Credentials{"root": root})
	}
	if local("/storage/emulated/0/") != local("/storage/emulated/0") {
		t.Error("trailing slash changed the local identity")
	}
}

// Without an identity we must not guess: two unrelated accounts would be
// silently merged and one of them deleted.
func TestIdentityKeyEmptyWhenUnknown(t *testing.T) {
	if identityKey(provider.KindGoogleDrive, provider.Credentials{}) != "" {
		t.Error("an OAuth account with no known identity produced a key")
	}
	if identityKey(provider.Kind("nonsense"), provider.Credentials{"x": "y"}) != "" {
		t.Error("an unknown provider produced a key")
	}
}

// Connecting the same drive twice must refresh it, not add a second copy —
// which is what every retried OAuth attempt used to do.
func TestReconnectingDoesNotDuplicate(t *testing.T) {
	h := newHarness(t)

	first := h.connectDAV()
	var accounts []map[string]any
	h.mustJSON(http.MethodGet, "/api/accounts", nil, &accounts)
	if len(accounts) != 1 {
		t.Fatalf("want 1 account after the first connect, got %d", len(accounts))
	}

	second := h.connectDAV() // same URL and username
	h.mustJSON(http.MethodGet, "/api/accounts", nil, &accounts)
	if len(accounts) != 1 {
		t.Fatalf("reconnecting created a duplicate: %d accounts", len(accounts))
	}
	if first != second {
		t.Errorf("reconnect returned a new account id (%s -> %s); it should reuse the existing one",
			first, second)
	}
}

// A setup that already accumulated duplicates gets repaired on start.
func TestDedupeAccountsMergesExistingDuplicates(t *testing.T) {
	h := newHarness(t)

	// Bypass the connect path to plant duplicates the way older builds did.
	creds := provider.Credentials{"url": h.davURL, "username": "user", "password": "pass"}
	for i := 0; i < 3; i++ {
		if _, err := h.st.AddAccount(provider.KindWebDAV, "Copy", creds); err != nil {
			t.Fatal(err)
		}
	}
	// Plus an unrelated one, which must survive untouched.
	if _, err := h.st.AddAccount(provider.KindWebDAV, "Different",
		provider.Credentials{"url": "https://elsewhere/dav", "username": "user"}); err != nil {
		t.Fatal(err)
	}
	if got := len(h.st.Accounts()); got != 4 {
		t.Fatalf("setup: want 4 accounts, got %d", got)
	}

	srv := New(Options{Version: "test", Store: h.st})
	removed := srv.DedupeAccounts()

	if len(removed) != 2 {
		t.Fatalf("removed %d duplicates, want 2", len(removed))
	}
	remaining := h.st.Accounts()
	if len(remaining) != 2 {
		t.Fatalf("want 2 accounts left, got %d", len(remaining))
	}
	var kept, unrelated bool
	for _, a := range remaining {
		if a.Creds["url"] == h.davURL {
			kept = true
		}
		if a.Label == "Different" {
			unrelated = true
		}
	}
	if !kept || !unrelated {
		t.Fatalf("dedupe removed the wrong accounts: %+v", remaining)
	}

	// Running again is a no-op.
	if again := srv.DedupeAccounts(); len(again) != 0 {
		t.Errorf("second pass removed %d more accounts", len(again))
	}
}
