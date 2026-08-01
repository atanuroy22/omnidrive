package provider

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// The cookie is the whole account, and people paste it in whatever shape their
// browser gave it to them. Every one of these is a real thing to arrive in that
// field, and getting any of them wrong locks the user out of a 1 TB drive with
// no way to tell why.
func TestNormalizeTeraboxCookie(t *testing.T) {
	const value = testTeraboxSession

	cases := map[string]string{
		// The bare value, which is what the TeraBox help pages point people at.
		value: "ndus=" + value,
		// Copied with its name attached.
		"ndus=" + value: "ndus=" + value,
		// Whitespace and quotes from a JSON viewer.
		`  "ndus=` + value + `"  `: "ndus=" + value,

		// A whole Cookie header. Everything TeraBox set is kept — an allow-list
		// of the cookies that *looked* important silently dropped ones the API
		// checks, and the account then authenticated as nobody. Only analytics
		// is discarded.
		"browserid=abc; ndus=" + value + "; _ga=GA1.1.9; csrfToken=zz; PANWEB=1": "browserid=abc; ndus=" + value + "; csrfToken=zz; PANWEB=1",

		// Order must not matter: an unknown cookie first used to change how the
		// whole header was parsed.
		"PANWEB=1; ndus=" + value: "PANWEB=1; ndus=" + value,
	}
	for in, want := range cases {
		if got := normalizeTeraboxCookie(in); got != want {
			t.Errorf("normalizeTeraboxCookie(%q) = %q, want %q", in, got, want)
		}
	}

	// Anything without a real session must be refused here rather than saved as
	// a drive that fails on every call. The short values are the ones that
	// matter: TeraBox hands a signed-out visitor a cookie by the same name, and
	// accepting one produced a connection that reported "your sign-in expired"
	// to somebody who had just signed in.
	for _, in := range []string{
		"", "   ",
		"browserid=abc; _ga=GA1.1.9", // no session at all
		"ndus=",                      // present but empty
		"ndus=null",
		"ndus=1",                            // visitor stub
		"browserid=abc; ndus=xyz; PANWEB=1", // too short to be real
	} {
		if got := normalizeTeraboxCookie(in); got != "" {
			t.Errorf("normalizeTeraboxCookie(%q) = %q, want a refusal", in, got)
		}
	}
}

func TestTeraboxPathAndChild(t *testing.T) {
	for in, want := range map[string]string{
		"":            "/",
		"/":           "/",
		"/a/b":        "/a/b",
		"a/b":         "/a/b",
		"/a/b/":       "/a/b",
		"  /a/b  ":    "/a/b",
		"/Videos/x.m": "/Videos/x.m",
	} {
		if got := teraboxPath(in); got != want {
			t.Errorf("teraboxPath(%q) = %q, want %q", in, got, want)
		}
	}

	if got, err := teraboxChild("", "clip.mp4"); err != nil || got != "/clip.mp4" {
		t.Errorf("child of root = %q, %v; want /clip.mp4", got, err)
	}
	if got, err := teraboxChild("/Videos", "clip.mp4"); err != nil || got != "/Videos/clip.mp4" {
		t.Errorf("child = %q, %v; want /Videos/clip.mp4", got, err)
	}
	// A separator in the name would write outside the chosen folder.
	if _, err := teraboxChild("/Videos", "a/b.mp4"); err == nil {
		t.Error("a name containing a separator was accepted")
	}
}

// A name collision is not a failure for Mkdir, so it has to stay recognisable
// through the error mapping rather than becoming prose.
func TestTeraboxErrorMapping(t *testing.T) {
	if !errors.Is(teraboxError(-8, ""), errTeraboxExists) {
		t.Error("errno -8 no longer reports an existing name")
	}
	if !errors.Is(teraboxError(-9, ""), ErrNotFound) {
		t.Error("errno -9 must map to ErrNotFound so the API answers 404")
	}
	if msg := teraboxError(999123, "").Error(); !strings.Contains(msg, "999123") {
		t.Errorf("an unmapped code should still be reported: %q", msg)
	}
}

// TeraBox answers -6 both to a stale cookie and to a request whose page token it
// could not read. Reporting the first for the second told people who had just
// signed in that their sign-in had expired, which sent them round the same loop.
func TestTeraboxSeparatesTheTwoMeaningsOfErrno6(t *testing.T) {
	tb := &terabox{base: "https://www.1024terabox.com"}
	raw := teraboxError(-6, "")

	// We held a page token, so we did reach a signed-in page: the session
	// itself is what stopped working.
	expired := tb.explain(raw, true).Error()
	if !strings.Contains(expired, "no longer valid") || !strings.Contains(expired, "sign in") {
		t.Errorf("with a token, -6 should report a lapsed session: %q", expired)
	}

	// No token means we never reached one, so blaming expiry is wrong — and
	// naming the likely cause is the whole point.
	fresh := tb.explain(raw, false).Error()
	if strings.Contains(fresh, "no longer valid") {
		t.Errorf("without a token, -6 must not claim the session expired: %q", fresh)
	}
	if !strings.Contains(fresh, "before sign-in") || !strings.Contains(fresh, tb.base) {
		t.Errorf("without a token, -6 should name the cause and the host tried: %q", fresh)
	}

	// Everything else must pass through untouched.
	if got := tb.explain(errTeraboxExists, false); !errors.Is(got, errTeraboxExists) {
		t.Errorf("explain() altered an unrelated error: %v", got)
	}
}

// testTeraboxSession is the shape of a real ndus value: opaque and long. Short
// stand-ins are rejected on purpose — see TestNormalizeTeraboxCookie.
const testTeraboxSession = "Y3RkZW1vLXNlc3Npb24tdmFsdWU"

// newTestTeraBox points a driver at a stub server, with the tokens pre-filled
// so no page fetch is attempted.
func newTestTeraBox(t *testing.T, h http.Handler) (*terabox, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	drv, err := newTeraBox(Config{
		Creds: Credentials{"cookie": "ndus=" + testTeraboxSession, "domain": srv.URL},
		HTTP:  srv.Client(),
		Save:  func(Credentials) error { return nil },
	})
	if err != nil {
		t.Fatalf("newTeraBox: %v", err)
	}
	tb := drv.(*terabox)
	tb.jsToken = "stub"
	tb.tokenAt = time.Now()
	return tb, srv
}

func TestTeraboxListPaginatesAndSorts(t *testing.T) {
	var pages int
	tb, _ := newTestTeraBox(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/list" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		if r.URL.Query().Get("app_id") != teraboxAppID {
			t.Errorf("the app id was not sent: %v", r.URL.RawQuery)
		}
		pages++
		// One short page: the listing loop must stop rather than spin.
		writeJSONTest(w, map[string]any{"errno": 0, "list": []any{
			map[string]any{"fs_id": 12, "path": "/zeta.txt", "server_filename": "zeta.txt",
				"isdir": 0, "size": 9, "server_mtime": 1700000000},
			map[string]any{"fs_id": 13, "path": "/Albums", "server_filename": "Albums",
				"isdir": 1, "size": 0, "server_mtime": 1700000001},
		}})
	}))

	files, err := tb.List(context.Background(), "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if pages != 1 {
		t.Fatalf("expected one request, made %d", pages)
	}
	if len(files) != 2 {
		t.Fatalf("got %d entries, want 2", len(files))
	}
	// Folders first is what every file manager does and what the UI expects.
	if !files[0].IsDir || files[0].Name != "Albums" {
		t.Errorf("folders were not sorted first: %+v", files)
	}
	if files[1].ID != "/zeta.txt" {
		t.Errorf("the id must be the path, got %q", files[1].ID)
	}
	if files[1].Modified.IsZero() {
		t.Error("the modified time was dropped")
	}
}

// isdir arrives as a number from one endpoint and a string from another, and
// treating a folder as a file makes it un-openable.
func TestTeraboxIsDirTolerance(t *testing.T) {
	var e struct {
		A teraBool `json:"a"`
		B teraBool `json:"b"`
		C teraBool `json:"c"`
		D teraBool `json:"d"`
	}
	if err := json.Unmarshal([]byte(`{"a":1,"b":"1","c":0,"d":true}`), &e); err != nil {
		t.Fatal(err)
	}
	if !e.A || !e.B || e.C || !e.D {
		t.Errorf("isdir decoding is wrong: %+v", e)
	}
}

// The upload is three calls that have to agree with each other: the block list
// committed at the end is what TeraBox assembles the file from, so it must be
// the hashes of what was actually sent.
func TestTeraboxUploadCommitsRealBlockHashes(t *testing.T) {
	var uploaded []string
	var committed []string
	var commitPath string

	tb, _ := newTestTeraBox(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		form, _ := url.ParseQuery(string(body))

		switch {
		case r.URL.Path == "/api/precreate":
			if form.Get("path") != "/Docs/notes.txt" {
				t.Errorf("precreate path = %q", form.Get("path"))
			}
			writeJSONTest(w, map[string]any{"errno": 0, "uploadid": "U1"})

		case r.URL.Path == "/rest/2.0/pcs/file":
			// Which upload front-end to use; the stub nominates itself.
			writeJSONTest(w, map[string]any{"servers": []any{
				map[string]any{"server": "http://" + r.Host},
			}})

		case strings.HasSuffix(r.URL.Path, "/rest/2.0/pcs/superfile2"):
			if r.URL.Query().Get("uploadid") != "U1" {
				t.Errorf("block sent without the session: %v", r.URL.RawQuery)
			}
			uploaded = append(uploaded, r.URL.Query().Get("partseq"))
			// Answer with a hash of our own: the server's opinion is the one
			// the commit is checked against, so it must be preferred.
			writeJSONTest(w, map[string]any{"md5": "server-hash-" + r.URL.Query().Get("partseq")})

		case strings.HasPrefix(r.URL.Path, "/api/create"):
			commitPath = form.Get("path")
			_ = json.Unmarshal([]byte(form.Get("block_list")), &committed)
			writeJSONTest(w, map[string]any{"errno": 0, "fs_id": 77,
				"path": form.Get("path"), "server_filename": "notes.txt", "isdir": 0, "size": 5})

		default:
			t.Errorf("unexpected call to %s", r.URL.Path)
		}
	}))

	f, err := tb.Upload(context.Background(), "/Docs", "notes.txt", 5,
		strings.NewReader("hello"), nil)
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if len(uploaded) != 1 || uploaded[0] != "0" {
		t.Fatalf("blocks sent = %v, want one block numbered 0", uploaded)
	}
	if len(committed) != 1 || committed[0] != "server-hash-0" {
		t.Fatalf("committed block list = %v, want the hash the server reported", committed)
	}
	if commitPath != "/Docs/notes.txt" {
		t.Errorf("commit path = %q", commitPath)
	}
	if f.ID != "/Docs/notes.txt" {
		t.Errorf("returned id = %q, want the path", f.ID)
	}
}

// A trashed item is addressed by fs_id, not by path, so the ids the bin hands
// out have to survive the round trip to restore and purge.
func TestTeraboxTrashRoundTrip(t *testing.T) {
	var restored string
	tb, _ := newTestTeraBox(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		form, _ := url.ParseQuery(string(body))

		switch r.URL.Path {
		case "/api/recycle/list":
			writeJSONTest(w, map[string]any{"errno": 0, "list": []any{
				map[string]any{"fs_id": 4242, "path": "/Old/report.pdf",
					"server_filename": "report.pdf", "isdir": 0, "size": 12,
					"delete_time": 1700000500},
			}})
		case "/api/recycle/restore":
			restored = form.Get("fidlist")
			writeJSONTest(w, map[string]any{"errno": 0})
		default:
			t.Errorf("unexpected call to %s", r.URL.Path)
		}
	}))

	items, err := tb.ListTrash(context.Background())
	if err != nil {
		t.Fatalf("ListTrash: %v", err)
	}
	if len(items) != 1 || items[0].Name != "report.pdf" {
		t.Fatalf("trash listing = %+v", items)
	}
	if items[0].OriginalPath != "/Old/report.pdf" || items[0].Deleted.IsZero() {
		t.Errorf("the entry lost its context: %+v", items[0])
	}
	if err := tb.RestoreTrash(context.Background(), items[0].ID); err != nil {
		t.Fatalf("RestoreTrash: %v", err)
	}
	if restored != "[4242]" {
		t.Errorf("restore sent %q, want the fs_id from the listing", restored)
	}
}

// A stale page token is the most common transient failure, and retrying it once
// is the difference between a working drive and a red toast on every write.
func TestTeraboxRetriesStalePageToken(t *testing.T) {
	var calls int
	tb, _ := newTestTeraBox(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/main" {
			// The refreshed page, served after the first rejection.
			w.Write([]byte(`<script>fn%28%22aabbccddeeff00112233%22%29</script>`))
			return
		}
		calls++
		if calls == 1 {
			writeJSONTest(w, map[string]any{"errno": 4000023})
			return
		}
		writeJSONTest(w, map[string]any{"errno": 0, "total": 1099511627776, "used": 42})
	}))

	q, err := tb.Quota(context.Background())
	if err != nil {
		t.Fatalf("Quota after a stale token: %v", err)
	}
	if calls != 2 {
		t.Fatalf("made %d calls, want a single retry", calls)
	}
	if q.Total != 1099511627776 || q.Used != 42 {
		t.Fatalf("quota = %+v", q)
	}
}

func writeJSONTest(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
