package provider

import (
	"errors"
	"net/url"
	"strings"
	"testing"
)

// Several provider APIs take credentials as query parameters, so any error
// that echoes a URL will leak them. This was seen for real: a failed pCloud
// login surfaced the user's password in the app's error toast.
func TestRedactURLHidesCredentials(t *testing.T) {
	cases := []struct{ in, mustNotContain string }{
		{"https://api.pcloud.com/userinfo?getauth=1&password=hunter2&username=a%40b.com", "hunter2"},
		{"https://api.pcloud.com/listfolder?auth=SESSIONTOKEN&folderid=0", "SESSIONTOKEN"},
		{"https://oauth2.googleapis.com/token?code=AUTHCODE&client_secret=SHH", "AUTHCODE"},
		{"https://oauth2.googleapis.com/token?client_secret=SHH", "SHH"},
		{"https://x/?access_token=TOK", "TOK"},
		{"https://x/?refresh_token=RTOK", "RTOK"},
	}
	for _, tc := range cases {
		got := redactURL(tc.in)
		if strings.Contains(got, tc.mustNotContain) {
			t.Errorf("redactURL(%q) leaked %q:\n  %s", tc.in, tc.mustNotContain, got)
		}
		if !strings.Contains(got, "REDACTED") {
			t.Errorf("redactURL(%q) did not mark anything redacted: %s", tc.in, got)
		}
	}
}

// Non-sensitive parameters must survive, or errors become useless.
func TestRedactURLKeepsHarmlessParts(t *testing.T) {
	got := redactURL("https://api.pcloud.com/listfolder?auth=SECRET&folderid=42&nofiles=1")
	for _, want := range []string{"api.pcloud.com", "listfolder", "folderid=42", "nofiles=1"} {
		if !strings.Contains(got, want) {
			t.Errorf("redaction removed useful context %q: %s", want, got)
		}
	}
}

func TestRedactURLLeavesCleanURLsAlone(t *testing.T) {
	in := "https://graph.microsoft.com/v1.0/me/drive/root/children?$top=500"
	if got := redactURL(in); got != in {
		t.Errorf("clean URL was altered:\n got %s\nwant %s", got, in)
	}
}

// Transport failures wrap the whole URL, which is how the password escaped in
// the first place: the TLS error carried the full request URL.
func TestRedactErrScrubsTransportErrors(t *testing.T) {
	inner := errors.New("tls: failed to verify certificate: x509: certificate signed by unknown authority")
	err := &url.Error{
		Op:  "Get",
		URL: "https://api.pcloud.com/userinfo?getauth=1&password=hunter2&username=a%40b.com",
		Err: inner,
	}

	got := redactErr(err).Error()
	if strings.Contains(got, "hunter2") {
		t.Fatalf("password leaked through the transport error:\n  %s", got)
	}
	// The underlying cause must still be legible, and still unwrappable.
	if !strings.Contains(got, "unknown authority") {
		t.Errorf("redaction destroyed the diagnostic: %s", got)
	}
	if !errors.Is(redactErr(err), inner) {
		t.Error("redactErr broke the error chain")
	}
}

func TestRedactErrPassesThroughOtherErrors(t *testing.T) {
	plain := errors.New("something else")
	if got := redactErr(plain); got != plain {
		t.Errorf("unrelated error was rewritten: %v", got)
	}
	if redactErr(nil) != nil {
		t.Error("nil error should stay nil")
	}
}

// The API error type embeds a URL too.
func TestAPIErrorRedactsURL(t *testing.T) {
	e := &apiError{
		Status: 401,
		Body:   `{"result":2000}`,
		URL:    redactURL("https://api.pcloud.com/listfolder?auth=SESSIONTOKEN"),
	}
	if strings.Contains(e.Error(), "SESSIONTOKEN") {
		t.Fatalf("apiError leaked the session token: %s", e.Error())
	}
}
