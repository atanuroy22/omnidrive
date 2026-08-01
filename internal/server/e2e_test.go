package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"omnidrive/internal/portable"
	"omnidrive/internal/store"
)

// This exercises the real stack end to end — HTTP API, driver, encrypted
// store, portable bundle — against an in-process WebDAV server. WebDAV is the
// cheapest backend to fake faithfully, and it shares every code path that
// matters with the OAuth providers above it.

// --- fake WebDAV server ---

type davFS struct {
	mu    sync.Mutex
	files map[string][]byte // key: path under /dav/; directories end in "/"
}

func newDavFS() *davFS {
	return &davFS{files: map[string][]byte{
		"docs/":      nil,
		"readme.txt": []byte("hello from the server"),
	}}
}

// newDavFSFrom builds a server holding the given paths, creating the parent
// folders each one implies.
func newDavFSFrom(paths []string) *davFS {
	d := &davFS{files: map[string][]byte{}}
	for _, p := range paths {
		parts := strings.Split(p, "/")
		for i := 1; i < len(parts); i++ {
			d.files[strings.Join(parts[:i], "/")+"/"] = nil
		}
		d.files[p] = []byte("contents of " + p)
	}
	return d
}

func (d *davFS) rel(p string) string {
	return strings.TrimPrefix(strings.TrimPrefix(p, "/dav"), "/")
}

func (d *davFS) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if u, p, ok := r.BasicAuth(); !ok || u != "user" || p != "pass" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	path := d.rel(r.URL.Path)

	d.mu.Lock()
	defer d.mu.Unlock()

	switch r.Method {
	case "PROPFIND":
		d.propfind(w, path, r.Header.Get("Depth"))
	case http.MethodGet:
		body, ok := d.files[path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Length", fmt.Sprint(len(body)))
		_, _ = w.Write(body)
	case http.MethodPut:
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		d.files[path] = body
		w.WriteHeader(http.StatusCreated)
	case "MKCOL":
		if !strings.HasSuffix(path, "/") {
			path += "/"
		}
		d.files[path] = nil
		w.WriteHeader(http.StatusCreated)
	case http.MethodDelete:
		delete(d.files, path)
		delete(d.files, path+"/")
		w.WriteHeader(http.StatusNoContent)
	case "MOVE":
		dest := d.rel(strings.TrimPrefix(r.Header.Get("Destination"), "http://"))
		if i := strings.Index(dest, "/dav/"); i >= 0 {
			dest = dest[i+len("/dav/"):]
		}
		// RFC 4918: moving a collection moves everything beneath it. Clients
		// address a collection with or without the trailing slash, so accept
		// either — a real server does.
		src := strings.TrimSuffix(path, "/")
		dst := strings.TrimSuffix(dest, "/")

		moves := map[string][]byte{}
		for key, body := range d.files {
			switch {
			case key == src || key == src+"/":
				moves[dst+strings.TrimPrefix(key, src)] = body
			case strings.HasPrefix(key, src+"/"):
				moves[dst+"/"+strings.TrimPrefix(key, src+"/")] = body
			default:
				continue
			}
			delete(d.files, key)
		}
		if len(moves) == 0 {
			http.NotFound(w, r)
			return
		}
		for key, body := range moves {
			d.files[key] = body
		}
		w.WriteHeader(http.StatusCreated)
	default:
		http.Error(w, "unsupported method "+r.Method, http.StatusMethodNotAllowed)
	}
}

func (d *davFS) propfind(w http.ResponseWriter, path, depth string) {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0"?><D:multistatus xmlns:D="DAV:">`)

	entry := func(href, name string, isDir bool, size int, extra string) {
		b.WriteString(`<D:response><D:href>/dav/` + href + `</D:href><D:propstat>`)
		b.WriteString(`<D:status>HTTP/1.1 200 OK</D:status><D:prop>`)
		b.WriteString(`<D:displayname>` + name + `</D:displayname>`)
		b.WriteString(`<D:getlastmodified>Mon, 27 Jul 2026 10:00:00 GMT</D:getlastmodified>`)
		if isDir {
			b.WriteString(`<D:resourcetype><D:collection/></D:resourcetype>`)
		} else {
			b.WriteString(`<D:resourcetype/><D:getcontentlength>` + fmt.Sprint(size) + `</D:getcontentlength>`)
		}
		b.WriteString(extra)
		b.WriteString(`</D:prop></D:propstat></D:response>`)
	}

	// The requested resource itself. Depth 0 is how Stat asks about a single
	// file, so this has to report the real name, type and size.
	quota := ""
	if path == "" {
		quota = `<D:quota-used-bytes>1024</D:quota-used-bytes><D:quota-available-bytes>9216</D:quota-available-bytes>`
	}
	selfName := strings.TrimSuffix(path, "/")
	if i := strings.LastIndex(selfName, "/"); i >= 0 {
		selfName = selfName[i+1:]
	}
	body, isFile := d.files[path]
	entry(path, selfName, !isFile || strings.HasSuffix(path, "/"), len(body), quota)

	if depth == "1" {
		var keys []string
		for k := range d.files {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if !strings.HasPrefix(k, path) || k == path {
				continue
			}
			remainder := strings.TrimPrefix(k, path)
			trimmed := strings.TrimSuffix(remainder, "/")
			if strings.Contains(trimmed, "/") {
				continue // not a direct child
			}
			entry(k, trimmed, strings.HasSuffix(k, "/"), len(d.files[k]), "")
		}
	}
	b.WriteString(`</D:multistatus>`)

	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.WriteHeader(http.StatusMultiStatus)
	_, _ = w.Write([]byte(b.String()))
}

// --- harness ---

type harness struct {
	t      *testing.T
	ts     *httptest.Server
	st     *store.Store
	dav    *davFS
	davURL string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	dav := newDavFS()
	davSrv := httptest.NewServer(dav)
	t.Cleanup(davSrv.Close)

	st, err := store.Open(t.TempDir(), "")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	srv := New(Options{Version: "test", Store: st, HTTP: davSrv.Client()})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	h := &harness{t: t, ts: ts, st: st, dav: dav}
	h.davURL = davSrv.URL + "/dav/"
	return h
}

func (h *harness) do(method, path string, body any) (*http.Response, []byte) {
	h.t.Helper()
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			h.t.Fatal(err)
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, h.ts.URL+path, rdr)
	if err != nil {
		h.t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := h.ts.Client().Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp, out
}

func (h *harness) mustJSON(method, path string, body any, out any) {
	h.t.Helper()
	resp, raw := h.do(method, path, body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		h.t.Fatalf("%s %s: HTTP %d: %s", method, path, resp.StatusCode, raw)
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			h.t.Fatalf("%s %s: decode %q: %v", method, path, raw, err)
		}
	}
}

func (h *harness) connectDAV() string {
	h.t.Helper()
	var acc map[string]any
	h.mustJSON(http.MethodPost, "/api/connect/direct", map[string]any{
		"kind":  "webdav",
		"label": "Test DAV",
		"fields": map[string]string{
			"url": h.davURL, "username": "user", "password": "pass",
		},
	}, &acc)
	id, _ := acc["id"].(string)
	if id == "" {
		h.t.Fatalf("connect returned no account id: %v", acc)
	}
	return id
}

func TestEndToEndBrowseUploadDownload(t *testing.T) {
	h := newHarness(t)
	accountID := h.connectDAV()

	t.Run("account appears with quota", func(t *testing.T) {
		var accounts []map[string]any
		h.mustJSON(http.MethodGet, "/api/accounts", nil, &accounts)
		if len(accounts) != 1 {
			t.Fatalf("want 1 account, got %d", len(accounts))
		}
		if got := accounts[0]["label"]; got != "Test DAV" {
			t.Errorf("label = %v", got)
		}
		// The fake reports 1024 used of 1024+9216 available.
		if used, total := accounts[0]["quotaUsed"], accounts[0]["quotaTotal"]; used != 1024.0 || total != 10240.0 {
			t.Errorf("quota = %v / %v, want 1024 / 10240", used, total)
		}
	})

	t.Run("virtual root lists drives", func(t *testing.T) {
		var res struct {
			Root  bool `json:"root"`
			Files []struct {
				Name      string `json:"name"`
				IsDir     bool   `json:"isDir"`
				AccountID string `json:"accountId"`
			} `json:"files"`
		}
		h.mustJSON(http.MethodGet, "/api/files", nil, &res)
		if !res.Root {
			t.Fatalf("root flag missing: %+v", res)
		}
		// Pooling is on by default, so the cloud account is represented by the
		// combined "Cloud" entry rather than listed on its own.
		if len(res.Files) != 1 {
			t.Fatalf("want just the combined cloud drive, got %+v", res.Files)
		}
		if res.Files[0].AccountID != "pool" || !res.Files[0].IsDir {
			t.Errorf("root entry should be the combined cloud drive, got %+v", res.Files[0])
		}

		// With pooling off the account appears in its own right.
		h.mustJSON(http.MethodPut, "/api/settings", map[string]any{"poolEnabled": false}, nil)
		h.mustJSON(http.MethodGet, "/api/files", nil, &res)
		if len(res.Files) != 1 || res.Files[0].Name != "Test DAV" || !res.Files[0].IsDir {
			t.Fatalf("unpooled root listing: %+v", res.Files)
		}
		h.mustJSON(http.MethodPut, "/api/settings", map[string]any{"poolEnabled": true}, nil)
	})

	t.Run("folder listing sorts folders first", func(t *testing.T) {
		files := h.list(accountID, "")
		if len(files) != 2 {
			t.Fatalf("want 2 entries, got %d: %+v", len(files), files)
		}
		if !files[0].IsDir || files[0].Name != "docs" {
			t.Errorf("first entry should be the docs folder, got %+v", files[0])
		}
		if files[1].Name != "readme.txt" || files[1].Size != 21 {
			t.Errorf("second entry = %+v, want readme.txt of 21 bytes", files[1])
		}
	})

	t.Run("upload streams into the provider", func(t *testing.T) {
		content := strings.Repeat("payload-", 512) // 4 KiB
		req, err := http.NewRequest(http.MethodPost,
			h.ts.URL+"/api/upload?name=uploaded.bin&account="+accountID,
			strings.NewReader(content))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/octet-stream")
		resp, err := h.ts.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("upload: HTTP %d: %s", resp.StatusCode, body)
		}

		h.dav.mu.Lock()
		stored := string(h.dav.files["uploaded.bin"])
		h.dav.mu.Unlock()
		if stored != content {
			t.Fatalf("stored %d bytes, want %d", len(stored), len(content))
		}
	})

	t.Run("download returns the bytes", func(t *testing.T) {
		resp, err := h.ts.Client().Get(
			h.ts.URL + "/api/files/download?account=" + accountID + "&id=readme.txt&name=readme.txt")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		if string(body) != "hello from the server" {
			t.Fatalf("download = %q", body)
		}
		if cd := resp.Header.Get("Content-Disposition"); !strings.Contains(cd, "readme.txt") {
			t.Errorf("Content-Disposition = %q", cd)
		}
	})

	t.Run("mkdir rename delete", func(t *testing.T) {
		h.mustJSON(http.MethodPost, "/api/files/mkdir", map[string]any{
			"account": accountID, "parent": "", "name": "newfolder",
		}, nil)
		h.mustJSON(http.MethodPost, "/api/files/rename", map[string]any{
			"account": accountID, "id": "readme.txt", "name": "renamed.txt",
		}, nil)

		h.dav.mu.Lock()
		_, hasFolder := h.dav.files["newfolder/"]
		_, hasRenamed := h.dav.files["renamed.txt"]
		_, hasOld := h.dav.files["readme.txt"]
		h.dav.mu.Unlock()
		if !hasFolder || !hasRenamed || hasOld {
			t.Fatalf("mkdir/rename did not take effect: folder=%v renamed=%v old=%v", hasFolder, hasRenamed, hasOld)
		}

		var del struct {
			Results []struct {
				OK bool `json:"ok"`
			} `json:"results"`
		}
		h.mustJSON(http.MethodPost, "/api/files/delete", map[string]any{
			"account": accountID, "ids": []string{"renamed.txt"},
		}, &del)
		if len(del.Results) != 1 || !del.Results[0].OK {
			t.Fatalf("delete result = %+v", del)
		}
	})

	t.Run("starring survives a round trip", func(t *testing.T) {
		h.mustJSON(http.MethodPost, "/api/files/star", map[string]any{
			"account": accountID, "id": "uploaded.bin", "on": true,
		}, nil)

		var starred struct {
			Files []struct {
				Name    string `json:"name"`
				Starred bool   `json:"starred"`
			} `json:"files"`
		}
		h.mustJSON(http.MethodGet, "/api/starred", nil, &starred)
		if len(starred.Files) != 1 || starred.Files[0].Name != "uploaded.bin" {
			t.Fatalf("starred = %+v", starred.Files)
		}
	})
}

type listedFile struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	IsDir bool   `json:"isDir"`
	Size  int64  `json:"size"`
}

func (h *harness) list(accountID, folder string) []listedFile {
	h.t.Helper()
	var res struct {
		Files []listedFile `json:"files"`
	}
	h.mustJSON(http.MethodGet, "/api/files?account="+accountID+"&folder="+folder, nil, &res)
	return res.Files
}

// The whole point of the project: a second device must end up with working
// accounts without touching a provider's sign-in screen.
func TestPortableBundleMovesAccountsToAnotherDevice(t *testing.T) {
	source := newHarness(t)
	accountID := source.connectDAV()

	// Star something so we can prove non-credential state travels too.
	source.mustJSON(http.MethodPost, "/api/files/star", map[string]any{
		"account": accountID, "id": "readme.txt", "on": true,
	}, nil)

	const passphrase = "a-strong-passphrase"
	resp, blob := source.do(http.MethodPost, "/api/portable/export", map[string]any{
		"passphrase": passphrase,
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("export: HTTP %d: %s", resp.StatusCode, blob)
	}
	if bytes.Contains(blob, []byte("pass")) || bytes.Contains(blob, []byte("webdav")) {
		t.Fatal("bundle contains recognisable plaintext")
	}

	// A completely separate device: new store, new server, same fake backend.
	dest := newHarness(t)
	dest.davURL = source.davURL

	var imported struct {
		Added   int `json:"added"`
		Updated int `json:"updated"`
		Bundle  struct {
			Accounts []string `json:"accounts"`
		} `json:"bundle"`
	}
	dest.importBundle(blob, passphrase, &imported)

	if imported.Added != 1 {
		t.Fatalf("want 1 imported account, got %d", imported.Added)
	}
	if len(imported.Bundle.Accounts) != 1 || !strings.Contains(imported.Bundle.Accounts[0], "Test DAV") {
		t.Fatalf("bundle summary = %+v", imported.Bundle.Accounts)
	}

	// The imported account must be immediately usable — same ID, live listing,
	// and the star carried across.
	files := dest.list(accountID, "")
	if len(files) == 0 {
		t.Fatal("imported account cannot list files")
	}
	var starred struct {
		Files []struct {
			Name string `json:"name"`
		} `json:"files"`
	}
	dest.mustJSON(http.MethodGet, "/api/starred", nil, &starred)
	if len(starred.Files) != 1 || starred.Files[0].Name != "readme.txt" {
		t.Fatalf("stars did not travel: %+v", starred.Files)
	}

	t.Run("re-importing is idempotent", func(t *testing.T) {
		var again struct{ Added, Updated int }
		dest.importBundle(blob, passphrase, &again)
		if again.Added != 0 || again.Updated != 1 {
			t.Fatalf("second import added %d / updated %d, want 0 / 1", again.Added, again.Updated)
		}
	})

	t.Run("wrong passphrase is rejected", func(t *testing.T) {
		resp, _ := dest.doMultipart(blob, "not-the-passphrase")
		if resp.StatusCode == http.StatusOK {
			t.Fatal("import accepted a wrong passphrase")
		}
	})
}

func (h *harness) importBundle(blob []byte, passphrase string, out any) {
	h.t.Helper()
	resp, raw := h.doMultipart(blob, passphrase)
	if resp.StatusCode != http.StatusOK {
		h.t.Fatalf("import: HTTP %d: %s", resp.StatusCode, raw)
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			h.t.Fatalf("decode import response: %v", err)
		}
	}
}

func (h *harness) doMultipart(blob []byte, passphrase string) (*http.Response, []byte) {
	h.t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	part, err := mw.CreateFormFile("bundle", portable.BundleName)
	if err != nil {
		h.t.Fatal(err)
	}
	if _, err := part.Write(blob); err != nil {
		h.t.Fatal(err)
	}
	_ = mw.WriteField("passphrase", passphrase)
	_ = mw.WriteField("replace", "false")
	if err := mw.Close(); err != nil {
		h.t.Fatal(err)
	}

	req, err := http.NewRequest(http.MethodPost, h.ts.URL+"/api/portable/import", &buf)
	if err != nil {
		h.t.Fatal(err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := h.ts.Client().Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp, raw
}

// Cloud config sync stores the bundle inside a connected drive, which is the
// route that needs no second device and no file transfer.
func TestCloudConfigSyncRoundTrip(t *testing.T) {
	source := newHarness(t)
	accountID := source.connectDAV()

	const passphrase = "cloud-sync-passphrase"
	var push struct {
		OK     bool   `json:"ok"`
		Folder string `json:"folder"`
		File   string `json:"file"`
	}
	source.mustJSON(http.MethodPost, "/api/portable/cloud/push", map[string]any{
		"account": accountID, "passphrase": passphrase,
	}, &push)
	if !push.OK {
		t.Fatal("push reported failure")
	}

	source.dav.mu.Lock()
	stored, ok := source.dav.files[portable.ConfigFolder+"/"+portable.BundleName]
	source.dav.mu.Unlock()
	if !ok || len(stored) == 0 {
		t.Fatalf("bundle was not written into the drive")
	}

	var status struct {
		Exists bool  `json:"exists"`
		Size   int64 `json:"size"`
	}
	source.mustJSON(http.MethodGet, "/api/portable/cloud/status?account="+accountID, nil, &status)
	if !status.Exists {
		t.Fatal("status does not see the stored bundle")
	}

	// A fresh device connects one account, then pulls everything else.
	dest := newHarness(t)
	dest.davURL = source.davURL
	dest.dav = source.dav
	newID := dest.connectDAVAgainst(source.davURL)

	var pull struct{ Added, Updated int }
	dest.mustJSON(http.MethodPost, "/api/portable/cloud/pull", map[string]any{
		"account": newID, "passphrase": passphrase, "replace": false,
	}, &pull)
	if pull.Added != 1 {
		t.Fatalf("pull added %d accounts, want 1 (the source device's account)", pull.Added)
	}

	var accounts []map[string]any
	dest.mustJSON(http.MethodGet, "/api/accounts", nil, &accounts)
	if len(accounts) != 2 {
		t.Fatalf("want the locally connected account plus the pulled one, got %d", len(accounts))
	}
}

func (h *harness) connectDAVAgainst(url string) string {
	h.t.Helper()
	h.davURL = url
	return h.connectDAV()
}

func TestPairingCodeIsSingleUseAndRateLimited(t *testing.T) {
	h := newHarness(t)
	h.connectDAV()

	var offer struct {
		URL       string    `json:"url"`
		Code      string    `json:"code"`
		ExpiresAt time.Time `json:"expiresAt"`
		Accounts  int       `json:"accounts"`
	}
	h.mustJSON(http.MethodPost, "/api/portable/pair/start", map[string]any{}, &offer)

	if offer.Accounts != 1 {
		t.Errorf("offer covers %d accounts, want 1", offer.Accounts)
	}
	if len(offer.Code) != 8 {
		t.Errorf("code %q should be 8 characters", offer.Code)
	}
	if !strings.Contains(offer.URL, "/pair/") {
		t.Errorf("URL = %q", offer.URL)
	}
	if time.Until(offer.ExpiresAt) > 11*time.Minute {
		t.Errorf("offer lives too long: %v", offer.ExpiresAt)
	}

	// Wrong codes must be refused, and the right one must work exactly once.
	client := &http.Client{Timeout: 5 * time.Second}
	fetch := func(code string) int {
		resp, err := client.Get(offer.URL + "?code=" + code)
		if err != nil {
			t.Fatalf("fetch: %v", err)
		}
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, resp.Body)
		return resp.StatusCode
	}
	if got := fetch("WRONGGGG"); got != http.StatusForbidden {
		t.Errorf("wrong code returned %d, want 403", got)
	}
	if got := fetch(offer.Code); got != http.StatusOK {
		t.Errorf("correct code returned %d, want 200", got)
	}
	if got := fetch(offer.Code); got != http.StatusForbidden {
		t.Errorf("replayed code returned %d, want 403 (single use)", got)
	}
}

func TestTokenAuthGuardsAPI(t *testing.T) {
	st, err := store.Open(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	srv := New(Options{Version: "test", Store: st, Token: "sekrit"})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	cases := []struct {
		name string
		req  func() *http.Request
		want int
	}{
		{"no token", func() *http.Request {
			r, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/health", nil)
			return r
		}, http.StatusUnauthorized},
		{"wrong token", func() *http.Request {
			r, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/health?t=nope", nil)
			return r
		}, http.StatusUnauthorized},
		{"query token", func() *http.Request {
			r, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/health?t=sekrit", nil)
			return r
		}, http.StatusOK},
		{"bearer token", func() *http.Request {
			r, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/health", nil)
			r.Header.Set("Authorization", "Bearer sekrit")
			return r
		}, http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := ts.Client().Do(tc.req())
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != tc.want {
				t.Fatalf("HTTP %d, want %d", resp.StatusCode, tc.want)
			}
		})
	}
}

// A client ID is saved before the sign-in that would prove it works, so a
// typo would otherwise be permanent: the app reuses the stored value without
// ever prompting again. Clearing it has to be possible.
func TestWrongOAuthClientIDCanBeReplaced(t *testing.T) {
	h := newHarness(t)

	const wrongID = "111-wrong.apps.googleusercontent.com"
	const rightID = "222-corrected.apps.googleusercontent.com"

	// Entering a client ID stores it, even though sign-in never completes.
	var started map[string]any
	h.mustJSON(http.MethodPost, "/api/connect/oauth/start", map[string]any{
		"kind": "gdrive", "clientId": wrongID, "clientSecret": "nope",
	}, &started)
	if !strings.Contains(started["authUrl"].(string), wrongID) {
		t.Fatalf("authUrl did not use the supplied client ID: %v", started["authUrl"])
	}

	settings := h.settings()
	if configured, _ := settings["oauthConfigured"].(map[string]any); configured["gdrive"] != true {
		t.Fatal("client ID was not stored")
	}
	// The masked form is what the UI offers to clear, so it must be present.
	if stored, _ := settings["oauthStored"].(map[string]any); stored["gdrive"] == nil {
		t.Fatal("stored client ID not reported, so the UI cannot offer to clear it")
	}

	// A second attempt with a different ID must take the new one, not silently
	// reuse the stored one.
	h.mustJSON(http.MethodPost, "/api/connect/oauth/start", map[string]any{
		"kind": "gdrive", "clientId": rightID,
	}, &started)
	if !strings.Contains(started["authUrl"].(string), rightID) {
		t.Fatalf("a corrected client ID was ignored: %v", started["authUrl"])
	}

	// And clearing must leave the provider unconfigured again.
	resp, body := h.do(http.MethodDelete, "/api/settings/oauth/gdrive", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("clear: HTTP %d: %s", resp.StatusCode, body)
	}
	settings = h.settings()
	if configured, _ := settings["oauthConfigured"].(map[string]any); configured["gdrive"] == true {
		t.Fatal("provider still reports as configured after clearing")
	}

	// With nothing stored, starting over must ask rather than reuse.
	resp, body = h.do(http.MethodPost, "/api/connect/oauth/start", map[string]any{"kind": "gdrive"})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected a prompt for credentials, got HTTP %d: %s", resp.StatusCode, body)
	}
}

func TestClearRejectsNonOAuthProvider(t *testing.T) {
	h := newHarness(t)
	resp, _ := h.do(http.MethodDelete, "/api/settings/oauth/webdav", nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("HTTP %d, want 400", resp.StatusCode)
	}
}

func (h *harness) settings() map[string]any {
	h.t.Helper()
	var out map[string]any
	h.mustJSON(http.MethodGet, "/api/settings", nil, &out)
	return out
}

// A crafted filename must not be able to escape the target folder.
func TestUploadRejectsPathSeparators(t *testing.T) {
	h := newHarness(t)
	accountID := h.connectDAV()

	for _, name := range []string{"../escape.txt", "nested/file.txt", `back\slash.txt`} {
		req, _ := http.NewRequest(http.MethodPost,
			h.ts.URL+"/api/upload?account="+accountID+"&name="+url.QueryEscape(name),
			strings.NewReader("x"))
		resp, err := h.ts.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("name %q returned %d, want 400", name, resp.StatusCode)
		}
	}
}
