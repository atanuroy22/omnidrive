package provider

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// pCloud identifies folders and files with separate numeric spaces, so we
// prefix our opaque IDs to keep them apart: "d123" is a folder, "f456" a file.
type pcloud struct {
	cfg  Config
	base string
}

func newPCloud(cfg Config) (Driver, error) {
	region := strings.ToLower(strings.TrimSpace(cfg.Creds["region"]))
	base := "https://eapi.pcloud.com"
	if region == "us" {
		base = "https://api.pcloud.com"
	}
	p := &pcloud{cfg: cfg, base: base}
	if cfg.Creds["auth"] == "" {
		if err := p.login(context.Background()); err != nil {
			return nil, err
		}
	}
	return p, nil
}

func (p *pcloud) Kind() Kind { return KindPCloud }

// form builds a POST with form-encoded parameters, keeping credentials out of
// the request URL.
func (p *pcloud) form(ctx context.Context, endpoint string, q url.Values) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.base+"/"+endpoint,
		strings.NewReader(q.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return req, nil
}

func randomBytes(n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand does not fail in practice, and a boundary needs
		// uniqueness rather than secrecy.
		for i := range b {
			b[i] = byte(i * 7)
		}
	}
	return b
}

// escapeQuotes makes a filename safe inside a Content-Disposition header.
func escapeQuotes(s string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\r", "", "\n", "").Replace(s)
}

// pcloudRegions are the two independent pCloud datacentres. An account exists
// in exactly one of them, and the wrong one simply reports a failed login — so
// rather than making the user know which they signed up on, try both.
var pcloudRegions = map[string]string{
	"eu": "https://eapi.pcloud.com",
	"us": "https://api.pcloud.com",
}

// login exchanges the stored username/password for a long-lived auth token so
// the password is only sent once per device.
func (p *pcloud) login(ctx context.Context) error {
	user, pass := p.cfg.Creds["username"], p.cfg.Creds["password"]
	if user == "" || pass == "" {
		return errors.New("pcloud: username and password are required")
	}

	// Configured region first, then the other one.
	hosts := []string{p.base}
	for _, h := range []string{pcloudRegions["eu"], pcloudRegions["us"]} {
		if h != p.base {
			hosts = append(hosts, h)
		}
	}

	var lastResult int
	var lastMsg string
	for _, host := range hosts {
		res, err := p.tryLogin(ctx, host, user, pass)
		if err != nil {
			return err // transport failure: no point trying the other region
		}
		if res.Result == 0 && res.Auth != "" {
			p.base = host
			p.cfg.Creds["auth"] = res.Auth
			p.cfg.Creds[CredAccountName] = res.Email
			for name, h := range pcloudRegions {
				if h == host {
					p.cfg.Creds["region"] = name
				}
			}
			// The password is no longer needed once we hold a token; dropping
			// it keeps the blast radius small if a backup is ever decrypted.
			delete(p.cfg.Creds, "password")
			return p.cfg.Save(p.cfg.Creds)
		}
		lastResult, lastMsg = res.Result, res.Error

		// 2321 "This user is on another location" is pCloud saying the account
		// lives in the other datacentre; 2000 is the older, vaguer "log in
		// failed" it returns in the same situation. Either means: try the other
		// region. Anything else is a real answer and is reported as-is.
		if res.Result != 2000 && res.Result != 2321 {
			break
		}
	}
	return pcloudLoginError(lastResult, lastMsg)
}

type pcloudLoginResponse struct {
	Result int    `json:"result"`
	Error  string `json:"error"`
	Auth   string `json:"auth"`
	Email  string `json:"email"`
}

// tryLogin authenticates against one datacentre.
//
// The endpoint is /login, not /userinfo: pCloud's userinfo no longer accepts a
// username and password, and answers one with result 1022 "Please provide
// 'code'" — meaning an OAuth code, not a two-factor code. That message is easy
// to misread as a 2FA prompt when it is really the wrong endpoint.
func (p *pcloud) tryLogin(ctx context.Context, host, user, pass string) (*pcloudLoginResponse, error) {
	q := url.Values{}
	q.Set("username", user)
	q.Set("password", pass)
	q.Set("getauth", "1")

	// POST, not GET: pCloud accepts either, but a GET would put the password in
	// the request URL, where it ends up in error messages and proxy logs.
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, host+"/login",
		strings.NewReader(q.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	var res pcloudLoginResponse
	if err := doJSON(p.cfg.HTTP, req, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

// pcloudLoginError turns pCloud's terse result codes into something a user can
// act on. The bare text for a 2FA-protected account is "Please provide 'code'",
// which tells you nothing about what to do next.
func pcloudLoginError(code int, msg string) error {
	switch code {
	// 1022 is a missing-parameter error, not a two-factor challenge: it means
	// the request went to an endpoint expecting an OAuth code. A genuine 2FA
	// challenge carries a token to continue with; this one carries nothing.
	case 1022:
		return errors.New("pCloud rejected the request as missing a parameter (code 1022). " +
			"This usually means the API changed; please report it")

	case 2000, 1000:
		return errors.New("pCloud rejected the email or password. Check both, and note that " +
			"an account exists in only one pCloud region — OmniDrive tries both automatically")

	case 2321:
		return errors.New("pCloud says this account lives in its other datacentre, and the " +
			"other one did not accept these details either — check the email and password")

	case 2012, 2064, 2074:
		return errors.New("pCloud wants a verification code. If two-factor authentication is on, " +
			"turn it off in pCloud → Settings → Security, or use a provider whose app passwords " +
			"work alongside 2FA — Koofr or Yandex over WebDAV, or Backblaze B2 over S3")

	case 4000:
		return errors.New("too many login attempts to pCloud; wait a few minutes and try again")
	}
	return fmt.Errorf("pcloud login failed: %s (code %d)", msg, code)
}

// call performs an API call, re-logging in once if the token has expired.
func (p *pcloud) call(ctx context.Context, endpoint string, q url.Values, out any) error {
	for attempt := 0; attempt < 2; attempt++ {
		if q == nil {
			q = url.Values{}
		}
		q.Set("auth", p.cfg.Creds["auth"])
		// Same reasoning as login: the session token must not travel in a URL.
		req, err := p.form(ctx, endpoint, q)
		if err != nil {
			return err
		}
		var probe struct {
			Result int    `json:"result"`
			Error  string `json:"error"`
		}
		body, err := readAll(p.cfg.HTTP, req)
		if err != nil {
			return err
		}
		if err := jsonUnmarshal(body, &probe); err != nil {
			return err
		}
		// 1000/2000/2094 are the "not logged in / invalid token" family.
		if probe.Result == 1000 || probe.Result == 2000 || probe.Result == 2094 {
			if attempt == 0 && p.cfg.Creds["password"] != "" {
				if lerr := p.login(ctx); lerr == nil {
					continue
				}
			}
			return fmt.Errorf("pcloud: session expired, reconnect the account (%s)", probe.Error)
		}
		if probe.Result != 0 {
			return fmt.Errorf("pcloud: %s (code %d)", probe.Error, probe.Result)
		}
		if out == nil {
			return nil
		}
		return jsonUnmarshal(body, out)
	}
	return errors.New("pcloud: request failed")
}

type pcMeta struct {
	Name     string   `json:"name"`
	IsFolder bool     `json:"isfolder"`
	FolderID int64    `json:"folderid"`
	FileID   int64    `json:"fileid"`
	Size     int64    `json:"size"`
	Modified string   `json:"modified"`
	Contents []pcMeta `json:"contents"`
}

func (m pcMeta) toFile() File {
	mod, _ := time.Parse(time.RFC1123Z, m.Modified)
	f := File{Name: m.Name, IsDir: m.IsFolder, Size: m.Size, Modified: mod}
	if m.IsFolder {
		f.ID = "d" + strconv.FormatInt(m.FolderID, 10)
	} else {
		f.ID = "f" + strconv.FormatInt(m.FileID, 10)
	}
	return f
}

// splitID decodes our prefixed identifier.
func splitPCloudID(id string) (isFolder bool, num string, err error) {
	if id == "" {
		return true, "0", nil // root folder
	}
	switch id[0] {
	case 'd':
		return true, id[1:], nil
	case 'f':
		return false, id[1:], nil
	}
	return false, "", fmt.Errorf("pcloud: malformed id %q", id)
}

func (p *pcloud) List(ctx context.Context, folderID string) ([]File, error) {
	isFolder, num, err := splitPCloudID(folderID)
	if err != nil || !isFolder {
		return nil, fmt.Errorf("pcloud: %q is not a folder", folderID)
	}
	var res struct {
		Metadata pcMeta `json:"metadata"`
	}
	q := url.Values{"folderid": {num}}
	if err := p.call(ctx, "listfolder", q, &res); err != nil {
		return nil, err
	}
	out := make([]File, 0, len(res.Metadata.Contents))
	for _, c := range res.Metadata.Contents {
		out = append(out, c.toFile())
	}
	SortFiles(out)
	return out, nil
}

func (p *pcloud) Stat(ctx context.Context, id string) (File, error) {
	isFolder, num, err := splitPCloudID(id)
	if err != nil {
		return File{}, err
	}
	var res struct {
		Metadata pcMeta `json:"metadata"`
	}
	endpoint, q := "stat", url.Values{"fileid": {num}}
	if isFolder {
		endpoint, q = "listfolder", url.Values{"folderid": {num}, "nofiles": {"1"}}
	}
	if err := p.call(ctx, endpoint, q, &res); err != nil {
		return File{}, err
	}
	return res.Metadata.toFile(), nil
}

func (p *pcloud) Download(ctx context.Context, id string) (io.ReadCloser, int64, error) {
	isFolder, num, err := splitPCloudID(id)
	if err != nil || isFolder {
		return nil, 0, fmt.Errorf("pcloud: %q is not a file", id)
	}
	var res struct {
		Hosts []string `json:"hosts"`
		Path  string   `json:"path"`
		Size  int64    `json:"size"`
	}
	if err := p.call(ctx, "getfilelink", url.Values{"fileid": {num}}, &res); err != nil {
		return nil, 0, err
	}
	if len(res.Hosts) == 0 {
		return nil, 0, errors.New("pcloud: no download host returned")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+res.Hosts[0]+res.Path, nil)
	if err != nil {
		return nil, 0, err
	}
	resp, err := p.cfg.HTTP.Do(req)
	if err != nil {
		return nil, 0, err
	}
	if err := checkResponse(resp); err != nil {
		return nil, 0, err
	}
	return resp.Body, resp.ContentLength, nil
}

// DownloadRange fetches a byte window from the temporary download host pCloud
// hands out, which serves plain HTTP and honours Range.
func (p *pcloud) DownloadRange(ctx context.Context, id string, start, end int64) (io.ReadCloser, error) {
	isFolder, num, err := splitPCloudID(id)
	if err != nil || isFolder {
		return nil, fmt.Errorf("pcloud: %q is not a file", id)
	}
	var res struct {
		Hosts []string `json:"hosts"`
		Path  string   `json:"path"`
	}
	if err := p.call(ctx, "getfilelink", url.Values{"fileid": {num}}, &res); err != nil {
		return nil, err
	}
	if len(res.Hosts) == 0 {
		return nil, errors.New("pcloud: no download host returned")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+res.Hosts[0]+res.Path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Range", rangeHeader(start, end))

	resp, err := p.cfg.HTTP.Do(req)
	if err != nil {
		return nil, redactErr(err)
	}
	if err := checkResponse(resp); err != nil {
		return nil, err
	}
	return resp.Body, nil
}

func (p *pcloud) Upload(ctx context.Context, parentID, name string, size int64, r io.Reader, prog Progress) (File, error) {
	isFolder, num, err := splitPCloudID(parentID)
	if err != nil || !isFolder {
		return File{}, fmt.Errorf("pcloud: upload target %q is not a folder", parentID)
	}

	// Build the multipart envelope by hand rather than with multipart.Writer
	// over an io.Pipe.
	//
	// A piped body has no known length, so Go falls back to chunked transfer
	// encoding — which pCloud's upload endpoint does not accept: it simply
	// never responds, and the request dies on the response-header timeout.
	// Assembling the envelope ourselves means the exact Content-Length is
	// known up front while the file itself still streams.
	boundary := "omnidrive" + hex.EncodeToString(randomBytes(16))
	prefix := "--" + boundary + "\r\n" +
		"Content-Disposition: form-data; name=\"file\"; filename=\"" + escapeQuotes(name) + "\"\r\n" +
		"Content-Type: application/octet-stream\r\n\r\n"
	suffix := "\r\n--" + boundary + "--\r\n"

	body := io.MultiReader(
		strings.NewReader(prefix),
		newProgressReader(r, prog),
		strings.NewReader(suffix),
	)

	q := url.Values{
		"folderid":  {num},
		"filename":  {name},
		"nopartial": {"1"},
		"auth":      {p.cfg.Creds["auth"]},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.base+"/uploadfile?"+q.Encode(), body)
	if err != nil {
		return File{}, err
	}
	req.Header.Set("Content-Type", "multipart/form-data; boundary="+boundary)
	if size >= 0 {
		req.ContentLength = int64(len(prefix)) + size + int64(len(suffix))
	}

	var res struct {
		Result   int      `json:"result"`
		Error    string   `json:"error"`
		Metadata []pcMeta `json:"metadata"`
	}
	if err := doJSON(p.cfg.HTTP, req, &res); err != nil {
		return File{}, err
	}
	if res.Result != 0 {
		return File{}, fmt.Errorf("pcloud upload: %s (code %d)", res.Error, res.Result)
	}
	if len(res.Metadata) == 0 {
		return File{}, errors.New("pcloud upload: no metadata returned")
	}
	return res.Metadata[0].toFile(), nil
}

func (p *pcloud) Mkdir(ctx context.Context, parentID, name string) (File, error) {
	isFolder, num, err := splitPCloudID(parentID)
	if err != nil || !isFolder {
		return File{}, fmt.Errorf("pcloud: %q is not a folder", parentID)
	}
	var res struct {
		Metadata pcMeta `json:"metadata"`
	}
	q := url.Values{"folderid": {num}, "name": {name}}
	// createfolderifnotexists is idempotent, which suits a retrying mobile client.
	if err := p.call(ctx, "createfolderifnotexists", q, &res); err != nil {
		return File{}, err
	}
	return res.Metadata.toFile(), nil
}

func (p *pcloud) Rename(ctx context.Context, id, newName string) error {
	isFolder, num, err := splitPCloudID(id)
	if err != nil {
		return err
	}
	if isFolder {
		return p.call(ctx, "renamefolder", url.Values{"folderid": {num}, "toname": {newName}}, nil)
	}
	return p.call(ctx, "renamefile", url.Values{"fileid": {num}, "toname": {newName}}, nil)
}

func (p *pcloud) Delete(ctx context.Context, id string) error {
	isFolder, num, err := splitPCloudID(id)
	if err != nil {
		return err
	}
	if isFolder {
		return p.call(ctx, "deletefolderrecursive", url.Values{"folderid": {num}}, nil)
	}
	return p.call(ctx, "deletefile", url.Values{"fileid": {num}}, nil)
}

// DeletePermanently removes the item and then purges that one entry from the
// trash, so the space comes back immediately. pCloud keeps the same id for a
// trashed item, which is what makes the second call possible; a plain delete
// would leave it counting against the quota until someone visits pcloud.com.
func (p *pcloud) DeletePermanently(ctx context.Context, id string) error {
	isFolder, num, err := splitPCloudID(id)
	if err != nil {
		return err
	}
	if err := p.Delete(ctx, id); err != nil {
		return err
	}
	key := "fileid"
	if isFolder {
		key = "folderid"
	}
	return p.call(ctx, "trash_clear", url.Values{key: {num}}, nil)
}

// EmptyTrash discards everything in pCloud's trash. Deleted files sit there
// counting against the quota until this runs.
func (p *pcloud) EmptyTrash(ctx context.Context) error {
	// folderid=0 is the trash root, so this clears all of it.
	return p.call(ctx, "trash_clear", url.Values{"folderid": {"0"}}, nil)
}

func (p *pcloud) Quota(ctx context.Context) (Quota, error) {
	var res struct {
		Quota     int64  `json:"quota"`
		UsedQuota int64  `json:"usedquota"`
		Email     string `json:"email"`
	}
	if err := p.call(ctx, "userinfo", nil, &res); err != nil {
		return Quota{}, err
	}
	if res.Email != "" && p.cfg.Creds[CredAccountName] != res.Email {
		p.cfg.Creds[CredAccountName] = res.Email
		_ = p.cfg.Save(p.cfg.Creds)
	}
	return Quota{Used: res.UsedQuota, Total: res.Quota}, nil
}

func (p *pcloud) accountName(ctx context.Context) string {
	if _, err := p.Quota(ctx); err != nil {
		return ""
	}
	return p.cfg.Creds[CredAccountName]
}
