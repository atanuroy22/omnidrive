package provider

import (
	"encoding/hex"
	"net/url"
	"testing"
)

// The signing-key chain is the part of SigV4 most likely to be silently wrong:
// a mistake produces a valid-looking signature that every server rejects. This
// is the worked example from the AWS "derive a signing key" documentation.
func TestSigV4SigningKeyDerivation(t *testing.T) {
	const (
		secret = "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"
		date   = "20120215"
		region = "us-east-1"
		svc    = "iam"
		want   = "f4780e2d9f65fa895f9c67b32ce1baf0b0d8a43505a000a1a9e090d414db404d"
	)
	kDate := hmacSHA256([]byte("AWS4"+secret), date)
	kRegion := hmacSHA256(kDate, region)
	kService := hmacSHA256(kRegion, svc)
	kSigning := hmacSHA256(kService, "aws4_request")

	if got := hex.EncodeToString(kSigning); got != want {
		t.Fatalf("signing key mismatch:\n got %s\nwant %s", got, want)
	}
}

func TestURIEncode(t *testing.T) {
	cases := map[string]string{
		"simple":       "simple",
		"with space":   "with%20space",
		"a/b":          "a%2Fb",
		"tilde~dash-":  "tilde~dash-",
		"under_dot.":   "under_dot.",
		"plus+amp&eq=": "plus%2Bamp%26eq%3D",
		"héllo":        "h%C3%A9llo", // UTF-8 bytes, percent-encoded individually
		"":             "",
	}
	for in, want := range cases {
		if got := uriEncode(in); got != want {
			t.Errorf("uriEncode(%q) = %q, want %q", in, got, want)
		}
	}
}

// Path separators must survive encoding or every nested key signs incorrectly.
func TestCanonicalURIKeepsSlashes(t *testing.T) {
	u, err := url.Parse("https://example.com/bucket/some folder/file+name.txt")
	if err != nil {
		t.Fatal(err)
	}
	want := "/bucket/some%20folder/file%2Bname.txt"
	if got := canonicalURI(u); got != want {
		t.Fatalf("canonicalURI = %q, want %q", got, want)
	}

	empty, _ := url.Parse("https://example.com")
	if got := canonicalURI(empty); got != "/" {
		t.Fatalf("empty path should canonicalise to /, got %q", got)
	}
}

// SigV4 requires query parameters sorted by name, then by value.
func TestCanonicalQuerySorts(t *testing.T) {
	u, err := url.Parse("https://x/?list-type=2&prefix=a%20b&delimiter=%2F&continuation-token=zz")
	if err != nil {
		t.Fatal(err)
	}
	want := "continuation-token=zz&delimiter=%2F&list-type=2&prefix=a%20b"
	if got := canonicalQuery(u); got != want {
		t.Fatalf("canonicalQuery =\n %q\nwant\n %q", got, want)
	}
}

func TestNewS3ValidatesInput(t *testing.T) {
	base := Credentials{"endpoint": "https://s3.example.com", "bucket": "b", "accessKey": "k", "secretKey": "s"}

	if _, err := newS3(Config{Creds: base}); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	for name, mutate := range map[string]func(Credentials){
		"missing bucket":     func(c Credentials) { delete(c, "bucket") },
		"missing access key": func(c Credentials) { delete(c, "accessKey") },
		"missing secret":     func(c Credentials) { delete(c, "secretKey") },
	} {
		creds := Credentials{}
		for k, v := range base {
			creds[k] = v
		}
		mutate(creds)
		if _, err := newS3(Config{Creds: creds}); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestS3KeyPrefixing(t *testing.T) {
	drv, err := newS3(Config{Creds: Credentials{
		"endpoint": "https://s3.example.com", "bucket": "b",
		"accessKey": "k", "secretKey": "s", "prefix": "nested/path",
	}})
	if err != nil {
		t.Fatal(err)
	}
	s := drv.(*s3)
	// A prefix without a trailing slash must gain one, or keys concatenate
	// into "nested/pathfile.txt".
	if s.prefix != "nested/path/" {
		t.Fatalf("prefix = %q, want %q", s.prefix, "nested/path/")
	}
	if got := s.key("docs/a.txt"); got != "nested/path/docs/a.txt" {
		t.Fatalf("key = %q", got)
	}
}
