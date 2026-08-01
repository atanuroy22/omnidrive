package provider

import (
	"context"
	"encoding/base64"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// webdav speaks generic WebDAV, which covers Nextcloud, ownCloud, Yandex Disk,
// Koofr, Box and most NAS software. Object IDs are the path relative to the
// configured base URL.
type webdav struct {
	cfg      Config
	base     *url.URL
	user     string
	pass     string
	basePath string
}

func newWebDAV(cfg Config) (Driver, error) {
	raw := strings.TrimSpace(cfg.Creds["url"])
	if raw == "" {
		return nil, errors.New("webdav: server URL is required")
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("webdav: bad URL: %w", err)
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return nil, fmt.Errorf("webdav: unsupported scheme %q", u.Scheme)
	}
	basePath := u.Path
	if !strings.HasSuffix(basePath, "/") {
		basePath += "/"
	}
	return &webdav{
		cfg:      cfg,
		base:     u,
		user:     cfg.Creds["username"],
		pass:     cfg.Creds["password"],
		basePath: basePath,
	}, nil
}

func (w *webdav) Kind() Kind { return KindWebDAV }

// urlFor maps a relative ID onto an absolute request URL.
func (w *webdav) urlFor(id string) string {
	rel := strings.TrimPrefix(id, "/")
	u := *w.base
	u.Path = w.basePath + rel
	// url.URL.String escapes Path correctly, including spaces and unicode.
	return u.String()
}

func (w *webdav) request(ctx context.Context, method, id string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, w.urlFor(id), body)
	if err != nil {
		return nil, err
	}
	if w.user != "" {
		req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(w.user+":"+w.pass)))
	}
	return req, nil
}

// multistatus mirrors the subset of RFC 4918 we care about.
type multistatus struct {
	XMLName   xml.Name `xml:"multistatus"`
	Responses []struct {
		Href     string `xml:"href"`
		Propstat []struct {
			Status string `xml:"status"`
			Prop   struct {
				DisplayName  string `xml:"displayname"`
				GetLastMod   string `xml:"getlastmodified"`
				GetLength    int64  `xml:"getcontentlength"`
				GetType      string `xml:"getcontenttype"`
				ResourceType struct {
					Collection *struct{} `xml:"collection"`
				} `xml:"resourcetype"`
				QuotaUsed      int64 `xml:"quota-used-bytes"`
				QuotaAvailable int64 `xml:"quota-available-bytes"`
			} `xml:"prop"`
		} `xml:"propstat"`
	} `xml:"response"`
}

const propfindBody = `<?xml version="1.0" encoding="utf-8"?>
<d:propfind xmlns:d="DAV:"><d:prop>
<d:displayname/><d:getlastmodified/><d:getcontentlength/><d:getcontenttype/><d:resourcetype/>
<d:quota-used-bytes/><d:quota-available-bytes/>
</d:prop></d:propfind>`

func (w *webdav) propfind(ctx context.Context, id, depth string) (*multistatus, error) {
	req, err := w.request(ctx, "PROPFIND", id, strings.NewReader(propfindBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Depth", depth)
	req.Header.Set("Content-Type", "application/xml; charset=utf-8")

	resp, err := w.cfg.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	if err := checkResponse(resp); err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var ms multistatus
	if err := xml.NewDecoder(resp.Body).Decode(&ms); err != nil {
		return nil, fmt.Errorf("webdav: parse PROPFIND response: %w", err)
	}
	return &ms, nil
}

func (w *webdav) List(ctx context.Context, folderID string) ([]File, error) {
	dir := folderID
	if dir != "" && !strings.HasSuffix(dir, "/") {
		dir += "/"
	}
	ms, err := w.propfind(ctx, dir, "1")
	if err != nil {
		return nil, err
	}

	selfPath := strings.TrimSuffix(w.basePath+strings.TrimPrefix(dir, "/"), "/")
	var out []File
	for _, r := range ms.Responses {
		href, err := url.PathUnescape(r.Href)
		if err != nil {
			href = r.Href
		}
		// Some servers return absolute URLs, others just paths.
		if u, err := url.Parse(href); err == nil && u.Host != "" {
			href = u.Path
		}
		if strings.TrimSuffix(href, "/") == selfPath {
			continue // the collection itself
		}
		rel := strings.TrimPrefix(strings.TrimPrefix(href, w.basePath), "/")
		if rel == "" {
			continue
		}
		f := File{ID: strings.TrimSuffix(rel, "/")}
		for _, ps := range r.Propstat {
			if !strings.Contains(ps.Status, "200") {
				continue
			}
			f.IsDir = ps.Prop.ResourceType.Collection != nil
			f.Size = ps.Prop.GetLength
			f.MIME = ps.Prop.GetType
			if t, err := time.Parse(time.RFC1123, ps.Prop.GetLastMod); err == nil {
				f.Modified = t
			}
			f.Name = ps.Prop.DisplayName
		}
		if f.Name == "" {
			parts := strings.Split(strings.TrimSuffix(rel, "/"), "/")
			f.Name = parts[len(parts)-1]
		}
		out = append(out, f)
	}
	SortFiles(out)
	return out, nil
}

func (w *webdav) Stat(ctx context.Context, id string) (File, error) {
	ms, err := w.propfind(ctx, id, "0")
	if err != nil {
		return File{}, err
	}
	if len(ms.Responses) == 0 {
		return File{}, ErrNotFound
	}
	r := ms.Responses[0]
	f := File{ID: strings.TrimSuffix(id, "/")}
	for _, ps := range r.Propstat {
		if !strings.Contains(ps.Status, "200") {
			continue
		}
		f.IsDir = ps.Prop.ResourceType.Collection != nil
		f.Size = ps.Prop.GetLength
		f.MIME = ps.Prop.GetType
		f.Name = ps.Prop.DisplayName
		if t, err := time.Parse(time.RFC1123, ps.Prop.GetLastMod); err == nil {
			f.Modified = t
		}
	}
	if f.Name == "" {
		parts := strings.Split(strings.TrimSuffix(id, "/"), "/")
		f.Name = parts[len(parts)-1]
	}
	return f, nil
}

func (w *webdav) Download(ctx context.Context, id string) (io.ReadCloser, int64, error) {
	req, err := w.request(ctx, http.MethodGet, id, nil)
	if err != nil {
		return nil, 0, err
	}
	resp, err := w.cfg.HTTP.Do(req)
	if err != nil {
		return nil, 0, err
	}
	if err := checkResponse(resp); err != nil {
		return nil, 0, err
	}
	return resp.Body, resp.ContentLength, nil
}

// DownloadRange requests a byte window; Range is part of plain HTTP GET, so
// every standards-compliant WebDAV server supports it.
func (w *webdav) DownloadRange(ctx context.Context, id string, start, end int64) (io.ReadCloser, error) {
	req, err := w.request(ctx, http.MethodGet, id, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Range", rangeHeader(start, end))

	resp, err := w.cfg.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	if err := checkResponse(resp); err != nil {
		return nil, err
	}
	return resp.Body, nil
}

func (w *webdav) Upload(ctx context.Context, parentID, name string, size int64, r io.Reader, p Progress) (File, error) {
	id := joinID(parentID, name)
	req, err := w.request(ctx, http.MethodPut, id, newProgressReader(r, p))
	if err != nil {
		return File{}, err
	}
	if size >= 0 {
		req.ContentLength = size
	}
	req.Header.Set("Content-Type", "application/octet-stream")

	resp, err := w.cfg.HTTP.Do(req)
	if err != nil {
		return File{}, err
	}
	if err := checkResponse(resp); err != nil {
		return File{}, err
	}
	resp.Body.Close()
	return File{ID: id, Name: name, Size: size, Modified: time.Now()}, nil
}

func (w *webdav) Mkdir(ctx context.Context, parentID, name string) (File, error) {
	id := joinID(parentID, name)
	req, err := w.request(ctx, "MKCOL", id+"/", nil)
	if err != nil {
		return File{}, err
	}
	resp, err := w.cfg.HTTP.Do(req)
	if err != nil {
		return File{}, err
	}
	// 405 means it already exists, which we treat as success.
	if resp.StatusCode != http.StatusMethodNotAllowed {
		if err := checkResponse(resp); err != nil {
			return File{}, err
		}
	}
	resp.Body.Close()
	return File{ID: id, Name: name, IsDir: true, Modified: time.Now()}, nil
}

func (w *webdav) Rename(ctx context.Context, id, newName string) error {
	parent := ""
	if i := strings.LastIndex(strings.TrimSuffix(id, "/"), "/"); i >= 0 {
		parent = id[:i]
	}
	req, err := w.request(ctx, "MOVE", id, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Destination", w.urlFor(joinID(parent, newName)))
	req.Header.Set("Overwrite", "F")

	resp, err := w.cfg.HTTP.Do(req)
	if err != nil {
		return err
	}
	if err := checkResponse(resp); err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

func (w *webdav) Delete(ctx context.Context, id string) error {
	req, err := w.request(ctx, http.MethodDelete, id, nil)
	if err != nil {
		return err
	}
	resp, err := w.cfg.HTTP.Do(req)
	if err != nil {
		return err
	}
	if err := checkResponse(resp); err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

func (w *webdav) Quota(ctx context.Context) (Quota, error) {
	ms, err := w.propfind(ctx, "", "0")
	if err != nil {
		return Quota{}, err
	}
	for _, r := range ms.Responses {
		for _, ps := range r.Propstat {
			if !strings.Contains(ps.Status, "200") {
				continue
			}
			used, avail := ps.Prop.QuotaUsed, ps.Prop.QuotaAvailable
			if avail < 0 {
				// Negative values are the RFC's way of saying "unlimited".
				return Quota{Used: used, Total: 0}, nil
			}
			if used > 0 || avail > 0 {
				return Quota{Used: used, Total: used + avail}, nil
			}
		}
	}
	return Quota{}, nil
}

// joinID concatenates path segments for the flat, path-based providers.
func joinID(parent, name string) string {
	parent = strings.Trim(parent, "/")
	name = strings.Trim(name, "/")
	if parent == "" {
		return name
	}
	return parent + "/" + name
}
