package provider

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// TeraBox.
//
// TeraBox gives away 1 TB, which makes it the largest free drive OmniDrive can
// connect — and the only one here with no OAuth of any kind for third parties.
// Its apps and its website both authenticate with a single session cookie,
// "ndus", so that is what an account stores. Everything else on this page
// follows from that one fact:
//
//   - There is no refresh token, so a connection lasts as long as the cookie
//     does (months in practice). When it finally lapses the API answers errno
//     -6 and the user signs in again — which is why every error path that can
//     mean "expired" says so in those words.
//   - Write calls additionally want "jsToken", a short-lived value stamped into
//     the logged-in web page rather than issued by an endpoint. We lift it from
//     the page HTML and refresh it whenever a call is rejected for it.
//   - The API is path-based, so a file's opaque ID here is simply its full
//     path. Numeric fs_ids exist and are needed for sharing and for the recycle
//     bin, but they are looked up on demand rather than being the identity.
type terabox struct {
	cfg  Config
	base string

	mu       sync.Mutex
	jsToken  string
	bdstoken string
	tokenAt  time.Time
	upload   string
}

const (
	// The app id every TeraBox client sends. Without it the API rejects the
	// request as malformed whatever else is correct.
	teraboxAppID = "250528"

	// Uploads are split into fixed 4 MiB blocks, each identified by its MD5.
	// The size is not negotiable: the commit step recomputes the boundaries and
	// refuses a block list that does not line up with them.
	teraboxBlock = 4 << 20

	// A *desktop* Chrome string, and deliberately so on a phone app.
	//
	// Two things depend on it. TeraBox's download CDN answers 403 with an empty
	// body to clients whose User-Agent it does not recognise, which reads as a
	// dead link rather than a refusal. And TeraBox redirects any mobile
	// User-Agent to /wap — an advertisement for its Android app — so the one
	// page that carries jsToken would never be reached, and every write would
	// fail. This is the desktop web client's API; it wants the desktop browser.
	teraboxUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
		"(KHTML, like Gecko) Chrome/122.0.0.0 Safari/537.36"

	// Where uploaded blocks go when TeraBox does not name a nearer front-end.
	teraboxUploadFallback = "https://c-jp.terabox.com"
)

// teraboxDomains are TeraBox's interchangeable front-ends, most-likely-to-work
// first. They share one backend and one cookie.
//
// The order matters more than it looks. TeraBox has migrated to
// www.1024terabox.com, and the original www.terabox.com now fails to resolve
// from a good many networks — India among them — so leading with the old name
// means a timeout before anything is tried. 1024tera.com and terabox.app both
// redirect to the current domain and are kept as the routes that still answer
// when the canonical one does not.
var teraboxDomains = []string{
	"https://www.1024terabox.com",
	"https://www.1024tera.com",
	"https://www.terabox.app",
	"https://www.terabox.com",
	"https://www.4funbox.com",
	"https://www.mirrobox.com",
}

// teraboxSignInURL is where someone signs in or signs up.
//
// It must be opened with a desktop User-Agent. TeraBox sends every mobile
// browser to /wap, which is a page advertising its Android app with no way to
// sign in at all; the same request from a desktop browser gets the real login
// form. (/wap/login, the obvious guess, now answers 404.)
const teraboxSignInURL = "https://www.1024terabox.com/login"

func newTeraBox(cfg Config) (Driver, error) {
	cookie := normalizeTeraboxCookie(cfg.Creds["cookie"])
	if cookie == "" {
		return nil, errors.New("terabox: no sign-in cookie. Use \"Sign in to TeraBox\" in the " +
			"Drives tab, or paste the ndus cookie from a signed-in browser")
	}
	cfg.Creds["cookie"] = cookie

	base := strings.TrimRight(strings.TrimSpace(cfg.Creds["domain"]), "/")
	if base == "" {
		base = teraboxDomains[0]
	}
	if !strings.Contains(base, "://") {
		base = "https://" + base
	}
	return &terabox{cfg: cfg, base: base}, nil
}

func (t *terabox) Kind() Kind { return KindTeraBox }

// ---------- credentials ----------

// teraboxDropCookies are the analytics cookies not worth storing. Everything
// else in the jar is kept.
//
// This used to be the other way round — an allow-list of the five cookies that
// looked like they mattered — which is a guess about someone else's private API
// dressed up as tidiness. TeraBox checks more than ndus, and a jar missing one
// of them authenticates as nobody, reported as errno -6: indistinguishable from
// an expired login. Keeping what the site set itself is both more correct and
// no less private, since a jar only ever contains that site's own cookies.
var teraboxDropCookies = map[string]bool{
	"_ga": true, "_gid": true, "_gat": true, "_fbp": true, "_uetsid": true,
	"_uetvid": true, "ab_sr": true, "__bid_n": true, "Hm_lvt": true, "Hm_lpvt": true,
}

// normalizeTeraboxCookie accepts any of the three things that actually arrive
// here — the bare ndus value, "ndus=...", or a whole Cookie header, either
// captured from the sign-in page or copied out of dev tools — and tidies it into
// a cookie header. It returns "" when there is no usable session in there, which
// the caller reports as a missing sign-in rather than letting it fail later as
// an authentication error.
func normalizeTeraboxCookie(raw string) string {
	raw = strings.TrimSpace(strings.Trim(strings.TrimSpace(raw), `"`))
	if raw == "" {
		return ""
	}
	// A bare value: one token, no separators, and not itself a "key=value"
	// pair. ndus values contain no '=' or ';' of their own.
	if !strings.ContainsAny(raw, "=;") {
		return "ndus=" + raw
	}

	var parts []string
	var haveSession bool
	for _, seg := range strings.Split(raw, ";") {
		k, v, ok := strings.Cut(strings.TrimSpace(seg), "=")
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if !ok || k == "" || v == "" || teraboxDropCookies[k] {
			continue
		}
		if k == "ndus" {
			// An empty or stub ndus is what TeraBox hands a visitor who has not
			// signed in. Treating one as a session produces a drive that
			// authenticates as nobody.
			if !plausibleTeraboxSession(v) {
				continue
			}
			haveSession = true
		}
		parts = append(parts, k+"="+v)
	}
	if !haveSession {
		return ""
	}
	return strings.Join(parts, "; ")
}

// plausibleTeraboxSession rejects values too short to be a real ndus. Genuine
// ones are opaque and around 40 characters; placeholders are a handful.
func plausibleTeraboxSession(v string) bool {
	return len(v) >= 16 && !strings.EqualFold(v, "null") && !strings.EqualFold(v, "undefined")
}

// ---------- request plumbing ----------

// params adds the query parameters TeraBox expects on every call.
func (t *terabox) params(extra url.Values) url.Values {
	q := url.Values{}
	q.Set("app_id", teraboxAppID)
	q.Set("web", "1")
	q.Set("channel", "dubox")
	q.Set("clienttype", "0")
	for k, vs := range extra {
		q[k] = vs
	}
	return q
}

func (t *terabox) newRequest(ctx context.Context, method, endpoint string, q, form url.Values) (*http.Request, error) {
	u := t.base + endpoint
	if len(q) > 0 {
		sep := "?"
		if strings.Contains(endpoint, "?") {
			sep = "&"
		}
		u += sep + q.Encode()
	}
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, err
	}
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	t.decorate(req)
	return req, nil
}

// decorate attaches the session and the browser identity TeraBox insists on.
func (t *terabox) decorate(req *http.Request) {
	req.Header.Set("Cookie", t.cfg.Creds["cookie"])
	req.Header.Set("User-Agent", teraboxUA)
	req.Header.Set("Referer", t.base+"/main?category=all")
}

// teraboxStatus is the envelope every endpoint wraps its answer in. The field
// is called errno on /api/ and error_code on the sharing endpoints, and a few
// return both, so all of them are read.
type teraboxStatus struct {
	Errno     *int   `json:"errno"`
	ErrorCode *int   `json:"error_code"`
	ErrorMsg  string `json:"error_msg"`
	ShowMsg   string `json:"show_msg"`
}

func (s teraboxStatus) code() int {
	if s.Errno != nil {
		return *s.Errno
	}
	if s.ErrorCode != nil {
		return *s.ErrorCode
	}
	return 0
}

func (s teraboxStatus) message() string {
	if s.ErrorMsg != "" {
		return s.ErrorMsg
	}
	return s.ShowMsg
}

// call performs one API request and unwraps the errno envelope.
//
// A rejected jsToken is retried once with a fresh one rather than surfaced:
// the token is minted by a web page and goes stale on its own schedule, so a
// single retry turns the most common transient failure into no failure at all.
func (t *terabox) call(ctx context.Context, method, endpoint string, q, form url.Values, out any) error {
	var last error
	var hadToken bool
	for attempt := 0; attempt < 2; attempt++ {
		p := t.params(q)
		js, _ := t.tokens(ctx)
		hadToken = js != ""
		if hadToken {
			p.Set("jsToken", js)
		}
		req, err := t.newRequest(ctx, method, endpoint, p, form)
		if err != nil {
			return err
		}
		body, err := readAll(t.cfg.HTTP, req)
		if err != nil {
			return err
		}

		var st teraboxStatus
		if err := jsonUnmarshal(body, &st); err != nil {
			return fmt.Errorf("terabox: unreadable answer from %s: %w", endpoint, err)
		}
		code := st.code()
		if teraboxStaleToken(code) && attempt == 0 {
			t.invalidateTokens()
			last = teraboxError(code, st.message())
			continue
		}
		if code != 0 {
			return t.explain(teraboxError(code, st.message()), hadToken)
		}
		if out == nil {
			return nil
		}
		return jsonUnmarshal(body, out)
	}
	if last == nil {
		last = errors.New("terabox: request failed")
	}
	return t.explain(last, hadToken)
}

// explain turns TeraBox's one ambiguous authentication code into the two things
// it separately means, because the fix is different for each and the failure is
// otherwise unactionable.
//
// -6 is returned both for "this cookie is not a signed-in session" and for "the
// page token on this request was missing or stale". We know which we sent, so we
// can say which it was.
func (t *terabox) explain(err error, hadToken bool) error {
	if !errors.Is(err, errTeraboxNotAuthorised) {
		return err
	}
	if hadToken {
		return errors.New("TeraBox rejected the sign-in. The saved session is no longer valid — " +
			"open the Drives tab and sign in to TeraBox again")
	}
	// No token means we never reached a signed-in page to read one from, so the
	// cookie is the thing at fault — most often captured before sign-in
	// finished, or pasted without the ndus value.
	return fmt.Errorf("TeraBox did not recognise this session (errno -6), and no page token could "+
		"be read from %s either. That usually means the cookie was copied before sign-in "+
		"finished. Sign in to TeraBox again, and on desktop check you copied the ndus value", t.base)
}

// teraboxStaleToken reports whether a code means "your page token expired"
// rather than "your sign-in expired". The two share errno -6, which is why a
// retry comes before any talk of signing in again.
func teraboxStaleToken(code int) bool {
	return code == -6 || code == 4000023 || code == 401
}

// errTeraboxExists is returned for a name collision. Mkdir treats it as success
// rather than an error, so it needs to be recognisable rather than just readable.
var errTeraboxExists = errors.New("something with that name is already there")

// errTeraboxNotAuthorised is TeraBox's errno -6. It is deliberately vague
// because the code is: call() recognises it and says which of the two things it
// actually means before the message reaches anyone.
var errTeraboxNotAuthorised = errors.New("TeraBox did not accept this session")

// teraboxError turns a numeric errno into something worth reading. The codes
// are inherited from the Baidu file API TeraBox is built on and carry no text
// of their own, so an unmapped one is reported as the number it is.
func teraboxError(code int, msg string) error {
	switch code {
	case -6, 401, 9019:
		// Deliberately not "your session expired". TeraBox answers -6 both to a
		// stale cookie and to a request whose page token it could not read, and
		// telling someone who signed in ten seconds ago that their sign-in
		// expired sends them round the same loop again. call() replaces this
		// with something more specific when it knows which case it hit.
		return errTeraboxNotAuthorised
	case -7:
		return errors.New(`TeraBox refused that name. Names may not contain \ / : * ? " < > |`)
	case -8, 4:
		return errTeraboxExists
	case -9, 31066, 31190:
		return fmt.Errorf("%w: TeraBox has no such file or folder any more", ErrNotFound)
	case -10, 36009:
		return errors.New("this TeraBox account is full — free space or delete from its recycle bin, " +
			"which keeps counting against the quota")
	case 2, 31023:
		return errors.New("TeraBox rejected the request as malformed (errno 2). If this keeps " +
			"happening the API has changed; please report it")
	case 12:
		return errors.New("TeraBox could not finish every item — some were left alone")
	case 111, 31034:
		return errors.New("TeraBox is rate-limiting this account. Wait a minute and try again")
	case 31021:
		return errors.New("TeraBox could not be reached from this network")
	}
	if msg != "" {
		return fmt.Errorf("terabox: %s (errno %d)", msg, code)
	}
	return fmt.Errorf("terabox: request failed (errno %d)", code)
}

// ---------- page tokens and identity ----------

var (
	// The logged-in page carries jsToken inside a URL-encoded fn("...") call.
	teraboxJsTokenRE = regexp.MustCompile(`fn%28%22([0-9A-Fa-f]{20,})%22%29`)
	// Some builds of the page emit it as ordinary JSON instead.
	teraboxJsTokenJSONRE = regexp.MustCompile(`"jsToken"\s*:\s*"([0-9A-Fa-f]{20,})"`)
	teraboxBdstokenRE    = regexp.MustCompile(`"bdstoken"\s*:\s*"([0-9a-f]{32})"`)
	teraboxUkRE          = regexp.MustCompile(`"uk"\s*:\s*(\d{3,})`)
	teraboxUsernameRE    = regexp.MustCompile(`"username"\s*:\s*"([^"]{1,80})"`)
)

// tokens returns the page tokens, fetching them at most once every ten minutes.
func (t *terabox) tokens(ctx context.Context) (jsToken, bdstoken string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.jsToken != "" && time.Since(t.tokenAt) < 10*time.Minute {
		return t.jsToken, t.bdstoken
	}
	home, err := t.fetchHome(ctx, t.base)

	// A page with no token on it means this front-end does not recognise the
	// session. Which of TeraBox's interchangeable domains serves a given account
	// is not something the user can be expected to know, so until one has worked
	// the others are tried and the one that answers is remembered.
	//
	// The stored domain is a hint, not a pin: the sign-in page records whichever
	// host it happened to set the cookie on, which is not always the host that
	// will serve the API for that account.
	if (err != nil || home.jsToken == "") && t.jsToken == "" {
		for _, alt := range teraboxDomains {
			if alt == t.base {
				continue
			}
			if page, aerr := t.fetchHome(ctx, alt); aerr == nil && page.jsToken != "" {
				t.base, home, err = alt, page, nil
				t.cfg.Creds["domain"] = alt
				_ = t.cfg.Save(t.cfg.Creds)
				break
			}
		}
	}
	if err != nil {
		// Keep whatever we had: a momentary network failure here must not turn
		// every subsequent call into an authentication error.
		return t.jsToken, t.bdstoken
	}
	t.tokenAt = time.Now()
	if home.jsToken != "" {
		t.jsToken = home.jsToken
	}
	if home.bdstoken != "" {
		t.bdstoken = home.bdstoken
	}
	t.rememberIdentityLocked(home)
	return t.jsToken, t.bdstoken
}

func (t *terabox) invalidateTokens() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.jsToken, t.tokenAt = "", time.Time{}
}

type teraboxHome struct {
	jsToken  string
	bdstoken string
	uk       string
	username string
}

// fetchHome loads the signed-in web page. It is the only place the tokens
// exist: TeraBox never hands them out from an endpoint.
func (t *terabox) fetchHome(ctx context.Context, base string) (teraboxHome, error) {
	var h teraboxHome
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/main?category=all", nil)
	if err != nil {
		return h, err
	}
	t.decorate(req)

	resp, err := t.cfg.HTTP.Do(req)
	if err != nil {
		return h, redactErr(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return h, &apiError{Status: resp.StatusCode, URL: base + "/main"}
	}
	// The page is around a megabyte; the cap is protection against a redirect
	// to something unbounded rather than a real expectation.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return h, err
	}
	page := string(body)

	if m := teraboxJsTokenRE.FindStringSubmatch(page); len(m) == 2 {
		h.jsToken = m[1]
	} else if m := teraboxJsTokenJSONRE.FindStringSubmatch(page); len(m) == 2 {
		h.jsToken = m[1]
	}
	if m := teraboxBdstokenRE.FindStringSubmatch(page); len(m) == 2 {
		h.bdstoken = m[1]
	}
	if m := teraboxUkRE.FindStringSubmatch(page); len(m) == 2 {
		h.uk = m[1]
	}
	if m := teraboxUsernameRE.FindStringSubmatch(page); len(m) == 2 {
		h.username = m[1]
	}
	return h, nil
}

// rememberIdentityLocked stores who we are signed in as. The numeric uk is what
// tells two TeraBox accounts apart, so reconnecting one updates it rather than
// listing the same 1 TB twice.
func (t *terabox) rememberIdentityLocked(h teraboxHome) {
	dirty := false
	if h.uk != "" && t.cfg.Creds["uk"] != h.uk {
		t.cfg.Creds["uk"] = h.uk
		dirty = true
	}
	name := h.username
	if name == "" && h.uk != "" {
		name = "TeraBox " + h.uk
	}
	if name != "" && t.cfg.Creds[CredAccountName] != name {
		t.cfg.Creds[CredAccountName] = name
		dirty = true
	}
	if dirty {
		_ = t.cfg.Save(t.cfg.Creds)
	}
}

func (t *terabox) accountName(ctx context.Context) string {
	t.tokens(ctx) // populates the identity as a side effect
	t.mu.Lock()
	defer t.mu.Unlock()
	return strings.TrimSpace(t.cfg.Creds[CredAccountName])
}

// ---------- entries ----------

// teraBool copes with isdir arriving as 1, "1" or true depending on which
// endpoint answered.
type teraBool bool

func (b *teraBool) UnmarshalJSON(p []byte) error {
	s := strings.Trim(string(p), `"`)
	*b = teraBool(s == "1" || s == "true")
	return nil
}

type teraEntry struct {
	FsID  json.Number `json:"fs_id"`
	Path  string      `json:"path"`
	Name  string      `json:"server_filename"`
	IsDir teraBool    `json:"isdir"`
	Size  int64       `json:"size"`
	MTime int64       `json:"server_mtime"`
	CTime int64       `json:"server_ctime"`
}

func (e teraEntry) toFile() File {
	f := File{
		ID:    e.Path,
		Name:  e.Name,
		IsDir: bool(e.IsDir),
		Size:  e.Size,
		Path:  e.Path,
	}
	if f.Name == "" {
		f.Name = path.Base(e.Path)
	}
	if f.IsDir {
		f.Size = 0
	}
	if e.MTime > 0 {
		f.Modified = time.Unix(e.MTime, 0).UTC()
	}
	return f
}

// teraboxPath maps our opaque ID onto a TeraBox path. The empty string is the
// account root, which TeraBox spells "/".
func teraboxPath(id string) string {
	id = strings.TrimSpace(id)
	if id == "" || id == "/" {
		return "/"
	}
	if !strings.HasPrefix(id, "/") {
		id = "/" + id
	}
	return strings.TrimRight(id, "/")
}

// teraboxChild joins a parent ID and a new name into a full path.
func teraboxChild(parentID, name string) (string, error) {
	if err := checkTeraboxName(name); err != nil {
		return "", err
	}
	parent := teraboxPath(parentID)
	if parent == "/" {
		return "/" + name, nil
	}
	return parent + "/" + name, nil
}

// checkTeraboxName rejects names TeraBox will refuse, so the user is told what
// is wrong instead of watching a transfer fail at the last step with errno -7.
func checkTeraboxName(name string) error {
	if strings.TrimSpace(name) == "" {
		return errors.New("terabox: a name is required")
	}
	if strings.ContainsAny(name, `\/:*?"<>|`) {
		return fmt.Errorf(`terabox: %q cannot be used — \ / : * ? " < > | are not allowed in a name`, name)
	}
	return nil
}

func mustJSONStrings(v []string) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// ---------- reading ----------

func (t *terabox) List(ctx context.Context, folderID string) ([]File, error) {
	const limit = 1000
	dir := teraboxPath(folderID)

	var out []File
	for start := 0; ; start += limit {
		var res struct {
			List []teraEntry `json:"list"`
		}
		q := url.Values{
			"dir":   {dir},
			"order": {"name"},
			"desc":  {"0"},
			"start": {strconv.Itoa(start)},
			"limit": {strconv.Itoa(limit)},
		}
		if err := t.call(ctx, http.MethodGet, "/api/list", q, nil, &res); err != nil {
			return nil, err
		}
		for _, e := range res.List {
			out = append(out, e.toFile())
		}
		if len(res.List) < limit {
			break
		}
	}
	SortFiles(out)
	return out, nil
}

// teraMeta is one entry from /api/filemetas, which is the same record as a
// listing plus the temporary download URL when it was asked for.
type teraMeta struct {
	teraEntry
	Dlink string `json:"dlink"`
}

func (t *terabox) meta(ctx context.Context, p string, wantDlink bool) (teraMeta, error) {
	var res struct {
		Info []teraMeta `json:"info"`
	}
	q := url.Values{
		"target": {mustJSONStrings([]string{p})},
		"dlink":  {"0"},
	}
	if wantDlink {
		q.Set("dlink", "1")
	}
	if err := t.call(ctx, http.MethodGet, "/api/filemetas", q, nil, &res); err != nil {
		return teraMeta{}, err
	}
	if len(res.Info) == 0 {
		return teraMeta{}, fmt.Errorf("%w: %s", ErrNotFound, p)
	}
	m := res.Info[0]
	if m.Path == "" {
		m.Path = p
	}
	return m, nil
}

func (t *terabox) Stat(ctx context.Context, id string) (File, error) {
	p := teraboxPath(id)
	if p == "/" {
		return File{ID: "", Name: "TeraBox", IsDir: true, Path: "/"}, nil
	}
	m, err := t.meta(ctx, p, false)
	if err != nil {
		return File{}, err
	}
	return m.toFile(), nil
}

func (t *terabox) Download(ctx context.Context, id string) (io.ReadCloser, int64, error) {
	resp, err := t.open(ctx, id, "")
	if err != nil {
		return nil, 0, err
	}
	return resp.Body, resp.ContentLength, nil
}

// DownloadRange fetches a byte window, which is what makes seeking in a video
// work instead of waiting for a two-hour film to arrive in full.
func (t *terabox) DownloadRange(ctx context.Context, id string, start, end int64) (io.ReadCloser, error) {
	resp, err := t.open(ctx, id, rangeHeader(start, end))
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

// open resolves the temporary CDN link for a file and starts reading it.
func (t *terabox) open(ctx context.Context, id, byteRange string) (*http.Response, error) {
	p := teraboxPath(id)
	if p == "/" {
		return nil, errors.New("terabox: the account root is a folder, not a file")
	}
	m, err := t.meta(ctx, p, true)
	if err != nil {
		return nil, err
	}
	if bool(m.IsDir) {
		return nil, fmt.Errorf("terabox: %q is a folder", p)
	}
	if m.Dlink == "" {
		return nil, errors.New("terabox: no download link was returned for that file")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.Dlink, nil)
	if err != nil {
		return nil, err
	}
	t.decorate(req)
	if byteRange != "" {
		req.Header.Set("Range", byteRange)
	}
	resp, err := t.cfg.HTTP.Do(req)
	if err != nil {
		return nil, redactErr(err)
	}
	if err := checkResponse(resp); err != nil {
		return nil, err
	}
	return resp, nil
}

// ---------- writing ----------

func (t *terabox) Mkdir(ctx context.Context, parentID, name string) (File, error) {
	dest, err := teraboxChild(parentID, name)
	if err != nil {
		return File{}, err
	}
	form := url.Values{
		"path":       {dest},
		"isdir":      {"1"},
		"block_list": {"[]"},
	}
	var res teraEntry
	err = t.call(ctx, http.MethodPost, "/api/create", nil, form, &res)

	// Creating a folder that is already there is not a failure anywhere else in
	// this app, and a retrying mobile client hits it constantly.
	if errors.Is(err, errTeraboxExists) {
		return t.Stat(ctx, dest)
	}
	if err != nil {
		return File{}, err
	}
	f := res.toFile()
	if f.ID == "" {
		f.ID, f.Path = dest, dest
	}
	f.IsDir = true
	if f.Name == "" {
		f.Name = name
	}
	return f, nil
}

func (t *terabox) Rename(ctx context.Context, id, newName string) error {
	p := teraboxPath(id)
	if p == "/" {
		return errors.New("terabox: the account root cannot be renamed")
	}
	if err := checkTeraboxName(newName); err != nil {
		return err
	}
	item, err := json.Marshal([]map[string]string{{"path": p, "newname": newName}})
	if err != nil {
		return err
	}
	return t.fileManager(ctx, "rename", string(item))
}

// Delete moves an item to TeraBox's recycle bin, where it goes on consuming the
// quota until the bin is emptied. DeletePermanently is the version that frees
// the space.
func (t *terabox) Delete(ctx context.Context, id string) error {
	p := teraboxPath(id)
	if p == "/" {
		return errors.New("terabox: the account root cannot be deleted")
	}
	return t.fileManager(ctx, "delete", mustJSONStrings([]string{p}))
}

// fileManager runs one of TeraBox's batch operations and waits for it when the
// server decides to run it in the background.
func (t *terabox) fileManager(ctx context.Context, opera, filelist string) error {
	form := url.Values{
		"async":    {"2"},
		"onnest":   {"fail"},
		"filelist": {filelist},
	}
	var res struct {
		TaskID json.Number `json:"taskid"`
		Info   []struct {
			Errno int    `json:"errno"`
			Path  string `json:"path"`
		} `json:"info"`
	}
	if err := t.call(ctx, http.MethodPost, "/api/filemanager?opera="+url.QueryEscape(opera),
		nil, form, &res); err != nil {
		return err
	}
	// A zero errno at the top with a failure inside info is how a partly
	// applied batch is reported.
	for _, i := range res.Info {
		if i.Errno != 0 {
			return teraboxError(i.Errno, "")
		}
	}
	if id := res.TaskID.String(); id != "" && id != "0" {
		return t.waitTask(ctx, id)
	}
	return nil
}

// waitTask polls a background file operation. TeraBox switches to one whenever
// the batch is large enough, and reports the outcome nowhere else.
func (t *terabox) waitTask(ctx context.Context, taskID string) error {
	deadline := time.Now().Add(10 * time.Minute)
	for attempt := 0; time.Now().Before(deadline); attempt++ {
		var res struct {
			Status    string `json:"status"`
			TaskErrno int    `json:"task_errno"`
		}
		q := url.Values{"taskid": {taskID}}
		if err := t.call(ctx, http.MethodGet, "/api/taskquery", q, nil, &res); err != nil {
			return err
		}
		switch res.Status {
		case "success", "":
			if res.TaskErrno != 0 {
				return teraboxError(res.TaskErrno, "")
			}
			return nil
		case "failed":
			return teraboxError(res.TaskErrno, "the operation failed on TeraBox")
		}
		if err := backoff(ctx, attempt); err != nil {
			return err
		}
	}
	return errors.New("terabox: the operation is still running on TeraBox — check the app in a moment")
}

// ---------- uploads ----------

// Upload writes a file in 4 MiB blocks.
//
// The three steps are TeraBox's, not ours: precreate opens a session, each
// block is posted to a separate upload front-end, and create commits the list
// of block MD5s into a real file. Only the commit list has to be accurate —
// precreate's copy exists so the server can spot blocks it already holds and
// skip them, so for a streamed file, whose contents we cannot know before
// reading them, it is filled with placeholders and the deduplication is simply
// forgone. A file that fits in one block is read into memory first, which makes
// its list exact and lets that optimisation happen.
func (t *terabox) Upload(ctx context.Context, parentID, name string, size int64, r io.Reader, prog Progress) (File, error) {
	dest, err := teraboxChild(parentID, name)
	if err != nil {
		return File{}, err
	}

	// TeraBox needs the length up front to plan the block list. A body of
	// unknown length is spooled to disk to learn it; that only happens for
	// uploads whose sender did not declare a size.
	if size < 0 {
		spool, spooledSize, err := spoolToTemp(r)
		if err != nil {
			return File{}, err
		}
		defer func() {
			spool.Close()
			os.Remove(spool.Name())
		}()
		r, size = spool, spooledSize
	}

	blocks := int((size + teraboxBlock - 1) / teraboxBlock)
	if blocks == 0 {
		blocks = 1 // an empty file is still one (empty) block
	}

	var first []byte
	precreateList := make([]string, blocks)
	if blocks == 1 {
		first, err = io.ReadAll(io.LimitReader(r, teraboxBlock+1))
		if err != nil {
			return File{}, err
		}
		if int64(len(first)) > teraboxBlock {
			return File{}, errors.New("terabox: the file is larger than its declared size")
		}
		size = int64(len(first))
		precreateList[0] = md5Hex(first)
	} else {
		// A placeholder that is the right shape but matches nothing, so every
		// block is asked for.
		for i := range precreateList {
			precreateList[i] = teraboxPlaceholderMD5
		}
	}

	var pre struct {
		UploadID   string    `json:"uploadid"`
		ReturnType int       `json:"return_type"`
		BlockList  []int     `json:"block_list"`
		Info       teraEntry `json:"info"`
	}
	form := url.Values{
		"path":       {dest},
		"size":       {strconv.FormatInt(size, 10)},
		"isdir":      {"0"},
		"autoinit":   {"1"},
		"rtype":      {"1"}, // rename rather than overwrite something already there
		"block_list": {mustJSONStrings(precreateList)},
	}
	if err := t.call(ctx, http.MethodPost, "/api/precreate", nil, form, &pre); err != nil {
		return File{}, err
	}
	// return_type 2 means TeraBox already held this exact content and linked it
	// straight in; there is nothing left to send.
	if pre.ReturnType == 2 {
		if prog != nil {
			prog(size)
		}
		if pre.Info.Path != "" {
			return pre.Info.toFile(), nil
		}
		return t.Stat(ctx, dest)
	}
	if pre.UploadID == "" {
		return File{}, errors.New("terabox: no upload session was returned")
	}

	host, err := t.uploadHost(ctx)
	if err != nil {
		return File{}, err
	}

	committed := make([]string, 0, blocks)
	var sent int64
	buf := make([]byte, teraboxBlock)

	for seq := 0; ; seq++ {
		var chunk []byte
		if seq == 0 && first != nil {
			chunk = first
		} else {
			n, readErr := io.ReadFull(r, buf)
			if readErr != nil && readErr != io.EOF && readErr != io.ErrUnexpectedEOF {
				return File{}, readErr
			}
			// A file whose length is an exact multiple of the block size ends
			// with an empty read; only the very first block may legitimately be
			// empty, and that is the empty-file case.
			if n == 0 && seq > 0 {
				break
			}
			chunk = buf[:n]
		}

		hash, err := t.uploadBlock(ctx, host, dest, pre.UploadID, seq, chunk)
		if err != nil {
			return File{}, err
		}
		committed = append(committed, hash)
		sent += int64(len(chunk))
		if prog != nil {
			prog(sent)
		}
		if len(chunk) < teraboxBlock {
			break
		}
	}

	if len(committed) == 0 {
		return File{}, errors.New("terabox: nothing was uploaded")
	}
	var res teraEntry
	commit := url.Values{
		"path":       {dest},
		"size":       {strconv.FormatInt(sent, 10)},
		"isdir":      {"0"},
		"rtype":      {"1"},
		"uploadid":   {pre.UploadID},
		"block_list": {mustJSONStrings(committed)},
	}
	if err := t.call(ctx, http.MethodPost, "/api/create?a=commit", nil, commit, &res); err != nil {
		return File{}, err
	}
	f := res.toFile()
	if f.ID == "" {
		f.ID, f.Path, f.Name, f.Size = dest, dest, name, sent
	}
	return f, nil
}

// teraboxPlaceholderMD5 stands in for a block whose contents are not known
// before it is read. It is the MD5 of the empty string, which no real 4 MiB
// block can collide with.
const teraboxPlaceholderMD5 = "d41d8cd98f00b204e9800998ecf8427e"

func md5Hex(b []byte) string {
	sum := md5.Sum(b)
	return hex.EncodeToString(sum[:])
}

// spoolToTemp writes an unmeasured stream to a temporary file and rewinds it.
func spoolToTemp(r io.Reader) (*os.File, int64, error) {
	f, err := os.CreateTemp("", "omnidrive-upload-*")
	if err != nil {
		return nil, 0, err
	}
	n, err := io.Copy(f, r)
	if err == nil {
		_, err = f.Seek(0, io.SeekStart)
	}
	if err != nil {
		f.Close()
		os.Remove(f.Name())
		return nil, 0, err
	}
	return f, n, nil
}

// uploadBlock posts one block and returns its MD5 as the server recorded it.
func (t *terabox) uploadBlock(ctx context.Context, host, dest, uploadID string, seq int, chunk []byte) (string, error) {
	q := url.Values{
		"method":   {"upload"},
		"type":     {"tmpfile"},
		"app_id":   {teraboxAppID},
		"channel":  {"dubox"},
		"web":      {"1"},
		"path":     {dest},
		"uploadid": {uploadID},
		"partseq":  {strconv.Itoa(seq)},
	}
	u := strings.TrimRight(host, "/") + "/rest/2.0/pcs/superfile2?" + q.Encode()

	// The envelope is assembled by hand so the exact Content-Length is known
	// while the block still streams: a piped body would fall back to chunked
	// transfer encoding, which this endpoint does not accept.
	boundary := "omnidrive" + hex.EncodeToString(randomBytes(16))
	prefix := "--" + boundary + "\r\n" +
		"Content-Disposition: form-data; name=\"file\"; filename=\"blob\"\r\n" +
		"Content-Type: application/octet-stream\r\n\r\n"
	suffix := "\r\n--" + boundary + "--\r\n"

	body := io.MultiReader(
		strings.NewReader(prefix),
		bytes.NewReader(chunk),
		strings.NewReader(suffix),
	)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, body)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "multipart/form-data; boundary="+boundary)
	req.ContentLength = int64(len(prefix) + len(chunk) + len(suffix))
	t.decorate(req)

	raw, err := readAll(t.cfg.HTTP, req)
	if err != nil {
		return "", err
	}
	var res struct {
		teraboxStatus
		MD5 string `json:"md5"`
	}
	if err := jsonUnmarshal(raw, &res); err != nil {
		return "", fmt.Errorf("terabox: unreadable answer while uploading block %d: %w", seq, err)
	}
	if code := res.code(); code != 0 {
		return "", teraboxError(code, res.message())
	}
	// Prefer the hash the server computed: the commit is checked against what
	// it actually stored, so its opinion is the one that has to match.
	if res.MD5 != "" {
		return res.MD5, nil
	}
	return md5Hex(chunk), nil
}

// uploadHost asks TeraBox which front-end to send blocks to. The answer is
// geographic, and a wrong one is slow rather than broken, so a failure here
// falls back to the default rather than aborting the upload.
func (t *terabox) uploadHost(ctx context.Context) (string, error) {
	t.mu.Lock()
	cached := t.upload
	t.mu.Unlock()
	if cached != "" {
		return cached, nil
	}

	host := teraboxUploadFallback
	var res struct {
		Host    string `json:"host"`
		Servers []struct {
			Server string `json:"server"`
		} `json:"servers"`
	}
	q := t.params(url.Values{"method": {"locateupload"}})
	if req, err := t.newRequest(ctx, http.MethodGet, "/rest/2.0/pcs/file", q, nil); err == nil {
		if body, err := readAll(t.cfg.HTTP, req); err == nil {
			_ = jsonUnmarshal(body, &res)
		}
	}
	switch {
	case len(res.Servers) > 0 && res.Servers[0].Server != "":
		host = res.Servers[0].Server
	case res.Host != "":
		host = res.Host
	}
	if !strings.Contains(host, "://") {
		host = "https://" + host
	}

	t.mu.Lock()
	t.upload = host
	t.mu.Unlock()
	return host, nil
}

// ---------- quota ----------

func (t *terabox) Quota(ctx context.Context) (Quota, error) {
	var res struct {
		Total int64 `json:"total"`
		Used  int64 `json:"used"`
	}
	q := url.Values{"checkfree": {"1"}}
	if err := t.call(ctx, http.MethodGet, "/api/quota", q, nil, &res); err != nil {
		return Quota{}, err
	}
	return Quota{Used: res.Used, Total: res.Total}, nil
}

// ---------- recycle bin ----------

// DeletePermanently removes an item and then purges that one entry from the
// recycle bin, so the space comes back at once. A plain Delete leaves it
// counting against the 1 TB until someone empties the bin, which on a drive
// this size is easy not to notice until it is full.
func (t *terabox) DeletePermanently(ctx context.Context, id string) error {
	// Purging something already in the bin: the trash listing identifies its
	// entries by fs_id, and there is no path to delete first.
	if fsID, ok := strings.CutPrefix(id, "fs:"); ok {
		return t.recycle(ctx, "delete", fsID)
	}

	// A live file: its fs_id has to be read before it is deleted, because the
	// bin is addressed by id and the path stops resolving once it moves.
	p := teraboxPath(id)
	m, err := t.meta(ctx, p, false)
	if err != nil {
		return err
	}
	if err := t.Delete(ctx, id); err != nil {
		return err
	}

	// From here the file is already gone as far as the user is concerned, so a
	// failure to clear the bin entry must not be reported as a failed delete —
	// it only means the space comes back when the bin is next emptied.
	if fsID := m.FsID.String(); fsID != "" {
		if err := t.recycle(ctx, "delete", fsID); err == nil {
			return nil
		}
	}
	// TeraBox does not promise that a binned item keeps its old id, so find it
	// again by the path it was deleted from.
	items, err := t.ListTrash(ctx)
	if err != nil {
		return nil
	}
	for _, it := range items {
		if it.OriginalPath == p {
			_ = t.recycle(ctx, "delete", strings.TrimPrefix(it.ID, "fs:"))
			return nil
		}
	}
	return nil
}

// EmptyTrash discards everything already deleted. Nothing else reclaims the
// space: TeraBox keeps trashed files against the quota indefinitely.
func (t *terabox) EmptyTrash(ctx context.Context) error {
	return t.call(ctx, http.MethodPost, "/api/recycle/clear", nil, url.Values{"async": {"1"}}, nil)
}

func (t *terabox) ListTrash(ctx context.Context) ([]TrashItem, error) {
	const num = 500
	var out []TrashItem

	for start := 0; ; start += num {
		var res struct {
			List []struct {
				teraEntry
				// Only some builds report this; where it is missing the entry's
				// modified time is when it was thrown away.
				DeleteTime int64 `json:"delete_time"`
			} `json:"list"`
		}
		q := url.Values{"start": {strconv.Itoa(start)}, "num": {strconv.Itoa(num)}}
		if err := t.call(ctx, http.MethodGet, "/api/recycle/list", q, nil, &res); err != nil {
			return nil, err
		}
		for _, e := range res.List {
			item := TrashItem{
				ID:           "fs:" + e.FsID.String(),
				Name:         e.Name,
				OriginalPath: e.Path,
				Size:         e.Size,
				IsDir:        bool(e.IsDir),
			}
			if item.Name == "" {
				item.Name = path.Base(e.Path)
			}
			when := e.DeleteTime
			if when == 0 {
				when = e.MTime
			}
			if when > 0 {
				item.Deleted = time.Unix(when, 0).UTC()
			}
			out = append(out, item)
		}
		if len(res.List) < num {
			break
		}
	}
	return out, nil
}

func (t *terabox) RestoreTrash(ctx context.Context, id string) error {
	fsID, ok := strings.CutPrefix(id, "fs:")
	if !ok {
		fsID = id
	}
	return t.recycle(ctx, "restore", fsID)
}

// recycle runs one of the bin operations, all of which address items by fs_id.
func (t *terabox) recycle(ctx context.Context, op, fsID string) error {
	if strings.TrimSpace(fsID) == "" {
		return errors.New("terabox: that item has no id to act on")
	}
	form := url.Values{
		"fidlist": {"[" + fsID + "]"},
		"async":   {"2"},
	}
	if op == "restore" {
		form.Set("ondup", "newcopy")
	}
	var res struct {
		TaskID json.Number `json:"taskid"`
	}
	if err := t.call(ctx, http.MethodPost, "/api/recycle/"+op, nil, form, &res); err != nil {
		return err
	}
	if tid := res.TaskID.String(); tid != "" && tid != "0" {
		return t.waitTask(ctx, tid)
	}
	return nil
}
