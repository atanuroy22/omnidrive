package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// Public share links.
//
// Every link here is minted by the provider, never by OmniDrive. That is not a
// shortcut — it is the only thing that works. This app runs on a phone behind
// carrier NAT with no fixed address, so a link it served itself would be
// unreachable from another network and would stop working the moment the screen
// went off. A provider link is served by their CDN, opens from anywhere, and
// keeps working when the phone is switched off entirely.
//
// The links are permanent: they last until revoked with Unshare. Anyone holding
// one can download the file without signing in, which is the point, and also
// the risk — the UI has to say so.

// ---------- Google Drive ----------

// ShareLink grants "anyone with the link" read access.
func (g *googleDrive) ShareLink(ctx context.Context, id string) (ShareLink, error) {
	esc := url.PathEscape(id)

	// Creating the permission twice is harmless — Google returns the existing
	// one — so there is no need to check first.
	u := fmt.Sprintf("%s/files/%s/permissions?supportsAllDrives=true", gdriveAPI, esc)
	err := g.doJSON(ctx, func() (*http.Request, error) {
		return jsonRequest(ctx, http.MethodPost, u, map[string]any{
			"role": "reader",
			"type": "anyone",
		})
	}, nil)
	if err != nil {
		return ShareLink{}, err
	}

	var meta struct {
		WebViewLink string `json:"webViewLink"`
		MimeType    string `json:"mimeType"`
	}
	mu := fmt.Sprintf("%s/files/%s?fields=webViewLink,mimeType&supportsAllDrives=true", gdriveAPI, esc)
	if err := g.doJSON(ctx, func() (*http.Request, error) {
		return http.NewRequestWithContext(ctx, http.MethodGet, mu, nil)
	}, &meta); err != nil {
		return ShareLink{}, err
	}

	link := ShareLink{URL: meta.WebViewLink}
	if link.URL == "" {
		link.URL = "https://drive.google.com/file/d/" + id + "/view"
	}
	// A folder has no single file to download, so it only gets the view link.
	if meta.MimeType != gdriveFolder {
		// drive.usercontent.google.com is where Google now serves file bytes,
		// and confirm=t skips the "we could not scan this for viruses" page it
		// shows for anything large. The webContentLink from the API still points
		// at the old /uc endpoint, which redirects here anyway.
		link.Direct = "https://drive.usercontent.google.com/download?id=" +
			url.QueryEscape(id) + "&export=download&confirm=t"
	}
	return link, nil
}

// Unshare removes the "anyone with the link" permission. Google gives that
// permission the fixed id "anyoneWithLink", so it needs no lookup.
func (g *googleDrive) Unshare(ctx context.Context, id string) error {
	u := fmt.Sprintf("%s/files/%s/permissions/anyoneWithLink?supportsAllDrives=true",
		gdriveAPI, url.PathEscape(id))
	return g.doJSON(ctx, func() (*http.Request, error) {
		return http.NewRequestWithContext(ctx, http.MethodDelete, u, nil)
	}, nil)
}

// ---------- OneDrive ----------

func (o *oneDrive) ShareLink(ctx context.Context, id string) (ShareLink, error) {
	if id == "" {
		return ShareLink{}, fmt.Errorf("onedrive: the drive root cannot be shared")
	}
	var res struct {
		Link struct {
			WebURL string `json:"webUrl"`
		} `json:"link"`
	}
	// scope "anonymous" is what makes it work without a Microsoft account.
	// Graph reuses an existing link for the same type and scope.
	u := graphAPI + itemPath(id) + "/createLink"
	if err := o.doJSON(ctx, func() (*http.Request, error) {
		return jsonRequest(ctx, http.MethodPost, u, map[string]any{
			"type": "view", "scope": "anonymous",
		})
	}, &res); err != nil {
		return ShareLink{}, err
	}
	if res.Link.WebURL == "" {
		return ShareLink{}, fmt.Errorf("onedrive: no link was returned — the account may block anonymous sharing")
	}
	// A "&download=1" on a OneDrive share URL skips the preview page.
	sep := "?"
	if strings.Contains(res.Link.WebURL, "?") {
		sep = "&"
	}
	return ShareLink{URL: res.Link.WebURL, Direct: res.Link.WebURL + sep + "download=1"}, nil
}

// Unshare deletes every anonymous link on the item. There can be more than one
// (a view link and an edit link), and leaving either behind would mean the file
// is still public after the user asked for it not to be.
func (o *oneDrive) Unshare(ctx context.Context, id string) error {
	var res struct {
		Value []struct {
			ID   string `json:"id"`
			Link *struct {
				Scope string `json:"scope"`
			} `json:"link"`
		} `json:"value"`
	}
	lu := graphAPI + itemPath(id) + "/permissions"
	if err := o.doJSON(ctx, func() (*http.Request, error) {
		return http.NewRequestWithContext(ctx, http.MethodGet, lu, nil)
	}, &res); err != nil {
		return err
	}
	for _, p := range res.Value {
		if p.Link == nil || p.Link.Scope != "anonymous" {
			continue
		}
		du := graphAPI + itemPath(id) + "/permissions/" + url.PathEscape(p.ID)
		if err := o.doJSON(ctx, func() (*http.Request, error) {
			return http.NewRequestWithContext(ctx, http.MethodDelete, du, nil)
		}, nil); err != nil {
			return err
		}
	}
	return nil
}

// ---------- Dropbox ----------

func (d *dropbox) ShareLink(ctx context.Context, id string) (ShareLink, error) {
	var res struct {
		URL string `json:"url"`
	}
	err := d.rpc(ctx, "https://api.dropboxapi.com/2/sharing/create_shared_link_with_settings",
		map[string]any{"path": id}, &res)

	// Dropbox refuses to mint a second link for a file that already has one,
	// and reports it as an error rather than returning the existing link.
	if err != nil && strings.Contains(err.Error(), "shared_link_already_exists") {
		var list struct {
			Links []struct {
				URL string `json:"url"`
			} `json:"links"`
		}
		if lerr := d.rpc(ctx, "https://api.dropboxapi.com/2/sharing/list_shared_links",
			map[string]any{"path": id, "direct_only": true}, &list); lerr != nil {
			return ShareLink{}, lerr
		}
		if len(list.Links) == 0 {
			return ShareLink{}, err
		}
		res.URL = list.Links[0].URL
	} else if err != nil {
		return ShareLink{}, err
	}

	return ShareLink{URL: res.URL, Direct: dropboxDirect(res.URL)}, nil
}

// dropboxDirect turns a preview link into one that downloads the file. Dropbox
// keys this off the dl query parameter.
func dropboxDirect(link string) string {
	if link == "" {
		return ""
	}
	u, err := url.Parse(link)
	if err != nil {
		return link
	}
	q := u.Query()
	q.Set("dl", "1")
	u.RawQuery = q.Encode()
	return u.String()
}

func (d *dropbox) Unshare(ctx context.Context, id string) error {
	var list struct {
		Links []struct {
			URL string `json:"url"`
		} `json:"links"`
	}
	if err := d.rpc(ctx, "https://api.dropboxapi.com/2/sharing/list_shared_links",
		map[string]any{"path": id, "direct_only": true}, &list); err != nil {
		return err
	}
	for _, l := range list.Links {
		if err := d.rpc(ctx, "https://api.dropboxapi.com/2/sharing/revoke_shared_link",
			map[string]any{"url": l.URL}, nil); err != nil {
			return err
		}
	}
	return nil
}

// ---------- pCloud ----------

func (p *pcloud) ShareLink(ctx context.Context, id string) (ShareLink, error) {
	isFolder, num, err := splitPCloudID(id)
	if err != nil {
		return ShareLink{}, err
	}
	endpoint, key := "getfilepublink", "fileid"
	if isFolder {
		endpoint, key = "getfolderpublink", "folderid"
	}

	var res struct {
		Link string `json:"link"`
		Code string `json:"code"`
	}
	// Deliberately not asking for shortlink=1. pCloud's short form is
	// http://u.pc.cd/XXX — served over cleartext HTTP with no TLS listener at
	// all, and its whole body is a JavaScript redirect to the real page. Phones
	// warn about it, Android blocks cleartext by default, and anything that does
	// not run the script gets a blank page instead of the file. Four characters
	// saved is not worth a link that does not download.
	if err := p.call(ctx, endpoint, url.Values{key: {num}}, &res); err != nil {
		return ShareLink{}, err
	}

	link := res.Link
	if link == "" && res.Code != "" {
		link = "https://u.pcloud.link/publink/show?code=" + url.QueryEscape(res.Code)
	}
	if link == "" {
		return ShareLink{}, fmt.Errorf("pcloud: no public link was returned")
	}
	// pCloud serves the file from the same page, so there is no separate
	// direct URL to offer.
	return ShareLink{URL: link}, nil
}

// ---------- TeraBox ----------

// ShareLink publishes a file at a permanent public URL.
//
// TeraBox addresses its sharing endpoints by numeric fs_id rather than by path,
// so the id has to be resolved first. period=0 asks for a link that never
// expires and schannel=0 for one with no password, which together are what
// "share a link" means everywhere else in this app.
func (t *terabox) ShareLink(ctx context.Context, id string) (ShareLink, error) {
	m, err := t.meta(ctx, teraboxPath(id), false)
	if err != nil {
		return ShareLink{}, err
	}
	fsID := m.FsID.String()
	if fsID == "" {
		return ShareLink{}, errors.New("terabox: that item has no id to share")
	}

	// Publishing the same file twice would litter the account with duplicate
	// links, and only the newest would be found again to revoke.
	if existing, _, err := t.findShare(ctx, fsID); err == nil && existing != "" {
		return ShareLink{URL: existing}, nil
	}

	form := url.Values{
		"fid_list":     {"[" + fsID + "]"},
		"schannel":     {"0"},
		"channel_list": {"[]"},
		"period":       {"0"},
	}
	var res struct {
		Link     string `json:"link"`
		ShortURL string `json:"shorturl"`
	}
	if err := t.share(ctx, "/share/set", form, &res); err != nil {
		return ShareLink{}, err
	}

	link := res.Link
	if link == "" && res.ShortURL != "" {
		link = t.base + "/s/" + res.ShortURL
	}
	if link == "" {
		return ShareLink{}, errors.New("terabox: no public link was returned")
	}
	// TeraBox serves the file from that page behind its own download button;
	// there is no separate direct URL to offer.
	return ShareLink{URL: link}, nil
}

func (t *terabox) Unshare(ctx context.Context, id string) error {
	m, err := t.meta(ctx, teraboxPath(id), false)
	if err != nil {
		return err
	}
	fsID := m.FsID.String()
	if fsID == "" {
		return errors.New("terabox: that item has no id")
	}
	_, shareID, err := t.findShare(ctx, fsID)
	if err != nil {
		return err
	}
	if shareID == "" {
		return errors.New("terabox: that item has no public link")
	}
	return t.share(ctx, "/share/cancel", url.Values{"shareid_list": {"[" + shareID + "]"}}, nil)
}

// findShare looks for an existing public link covering one fs_id, returning its
// URL and the share id needed to revoke it.
func (t *terabox) findShare(ctx context.Context, fsID string) (link, shareID string, err error) {
	const num = 100
	for page := 1; page <= 20; page++ {
		var res struct {
			List []struct {
				ShareID  json.Number   `json:"shareId"`
				ShortURL string        `json:"shorturl"`
				Link     string        `json:"link"`
				FsIDs    []json.Number `json:"fsIds"`
			} `json:"list"`
		}
		q := url.Values{
			"page":  {strconv.Itoa(page)},
			"num":   {strconv.Itoa(num)},
			"desc":  {"1"},
			"ptype": {"file"},
		}
		if err := t.call(ctx, http.MethodGet, "/share/list", q, nil, &res); err != nil {
			return "", "", err
		}
		for _, s := range res.List {
			for _, f := range s.FsIDs {
				if f.String() != fsID {
					continue
				}
				href := s.Link
				if href == "" && s.ShortURL != "" {
					href = t.base + "/s/" + s.ShortURL
				}
				return href, s.ShareID.String(), nil
			}
		}
		if len(res.List) < num {
			break
		}
	}
	return "", "", nil
}

// share posts to a /share/ endpoint. Those alone want the bdstoken lifted from
// the web page, and TeraBox has renamed the create endpoint at least once — so
// a parameter complaint from one spelling is retried with the other rather than
// shown to a user who can do nothing about it.
func (t *terabox) share(ctx context.Context, endpoint string, form url.Values, out any) error {
	q := url.Values{}
	if _, bd := t.tokens(ctx); bd != "" {
		q.Set("bdstoken", bd)
	}
	err := t.call(ctx, http.MethodPost, endpoint, q, form, out)
	if err != nil && endpoint == "/share/set" {
		if alt := t.call(ctx, http.MethodPost, "/share/create", q, form, out); alt == nil {
			return nil
		}
	}
	return err
}

func (p *pcloud) Unshare(ctx context.Context, id string) error {
	isFolder, num, err := splitPCloudID(id)
	if err != nil {
		return err
	}
	key := "fileid"
	if isFolder {
		key = "folderid"
	}

	// pCloud revokes by link id, so the file's links have to be found first.
	// The id of the shared item is nested inside metadata, not at the top level
	// of the publink record — reading it from the wrong place silently matches
	// nothing and every revoke reports "no public link".
	var links struct {
		Publinks []struct {
			LinkID   int64 `json:"linkid"`
			Metadata struct {
				FileID   int64 `json:"fileid"`
				FolderID int64 `json:"folderid"`
			} `json:"metadata"`
		} `json:"publinks"`
	}
	if err := p.call(ctx, "listpublinks", nil, &links); err != nil {
		return err
	}

	var found bool
	for _, l := range links.Publinks {
		match := (key == "fileid" && fmt.Sprint(l.Metadata.FileID) == num) ||
			(key == "folderid" && fmt.Sprint(l.Metadata.FolderID) == num)
		if !match {
			continue
		}
		found = true
		if err := p.call(ctx, "deletepublink",
			url.Values{"linkid": {fmt.Sprint(l.LinkID)}}, nil); err != nil {
			return err
		}
	}
	if !found {
		return fmt.Errorf("pcloud: that item has no public link")
	}
	return nil
}
