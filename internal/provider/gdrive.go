package provider

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	gdriveAPI    = "https://www.googleapis.com/drive/v3"
	gdriveUpload = "https://www.googleapis.com/upload/drive/v3"
	gdriveFolder = "application/vnd.google-apps.folder"
	// Fields we ask for on every listing. Requesting explicitly keeps
	// responses small, which matters on metered mobile data.
	gdriveFields = "id,name,mimeType,size,modifiedTime,trashed"
)

type googleDrive struct {
	*authClient
}

func newGoogleDrive(cfg Config) Driver {
	ep, _ := OAuthEndpoints(KindGoogleDrive)
	return &googleDrive{authClient: newAuthClient(cfg, ep)}
}

func (g *googleDrive) Kind() Kind { return KindGoogleDrive }

type gdriveFile struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	MimeType     string `json:"mimeType"`
	Size         string `json:"size"` // int64 as a string in the v3 API
	ModifiedTime string `json:"modifiedTime"`
	Trashed      bool   `json:"trashed"`
}

func (f gdriveFile) toFile() File {
	mod, _ := time.Parse(time.RFC3339, f.ModifiedTime)
	var size int64
	fmt.Sscanf(f.Size, "%d", &size)
	return File{
		ID:       f.ID,
		Name:     f.Name,
		IsDir:    f.MimeType == gdriveFolder,
		Size:     size,
		Modified: mod,
		MIME:     f.MimeType,
	}
}

func gdriveParent(folderID string) string {
	if folderID == "" {
		return "root"
	}
	return folderID
}

func (g *googleDrive) List(ctx context.Context, folderID string) ([]File, error) {
	var out []File
	pageToken := ""
	for {
		q := url.Values{}
		q.Set("q", fmt.Sprintf("'%s' in parents and trashed=false", gdriveParent(folderID)))
		q.Set("fields", "nextPageToken,files("+gdriveFields+")")
		q.Set("pageSize", "1000")
		q.Set("supportsAllDrives", "true")
		q.Set("includeItemsFromAllDrives", "true")
		if pageToken != "" {
			q.Set("pageToken", pageToken)
		}
		u := gdriveAPI + "/files?" + q.Encode()

		var res struct {
			NextPageToken string       `json:"nextPageToken"`
			Files         []gdriveFile `json:"files"`
		}
		err := g.doJSON(ctx, func() (*http.Request, error) {
			return http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		}, &res)
		if err != nil {
			return nil, err
		}
		for _, f := range res.Files {
			out = append(out, f.toFile())
		}
		if res.NextPageToken == "" {
			break
		}
		pageToken = res.NextPageToken
	}
	SortFiles(out)
	return out, nil
}

func (g *googleDrive) Stat(ctx context.Context, id string) (File, error) {
	u := fmt.Sprintf("%s/files/%s?fields=%s&supportsAllDrives=true", gdriveAPI, url.PathEscape(gdriveParent(id)), gdriveFields)
	var f gdriveFile
	err := g.doJSON(ctx, func() (*http.Request, error) {
		return http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	}, &f)
	if err != nil {
		return File{}, err
	}
	return f.toFile(), nil
}

func (g *googleDrive) Download(ctx context.Context, id string) (io.ReadCloser, int64, error) {
	u := fmt.Sprintf("%s/files/%s?alt=media&supportsAllDrives=true", gdriveAPI, url.PathEscape(id))
	resp, err := g.do(ctx, func() (*http.Request, error) {
		return http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	})
	if err != nil {
		return nil, 0, err
	}
	if err := checkResponse(resp); err != nil {
		return nil, 0, err
	}
	return resp.Body, resp.ContentLength, nil
}

// DownloadRange asks Drive for a byte window so playback can seek.
func (g *googleDrive) DownloadRange(ctx context.Context, id string, start, end int64) (io.ReadCloser, error) {
	u := fmt.Sprintf("%s/files/%s?alt=media&supportsAllDrives=true", gdriveAPI, url.PathEscape(id))
	resp, err := g.do(ctx, func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Range", rangeHeader(start, end))
		return req, nil
	})
	if err != nil {
		return nil, err
	}
	if err := checkResponse(resp); err != nil {
		return nil, err
	}
	return resp.Body, nil
}

func (g *googleDrive) Upload(ctx context.Context, parentID, name string, size int64, r io.Reader, p Progress) (File, error) {
	// Resumable uploads accept an unbounded stream in a single PUT, which
	// avoids buffering a multi-gigabyte video in phone RAM.
	meta := map[string]any{
		"name":    name,
		"parents": []string{gdriveParent(parentID)},
	}
	initURL := gdriveUpload + "/files?uploadType=resumable&supportsAllDrives=true&fields=" + gdriveFields

	resp, err := g.do(ctx, func() (*http.Request, error) {
		return jsonRequest(ctx, http.MethodPost, initURL, meta)
	})
	if err != nil {
		return File{}, err
	}
	if err := checkResponse(resp); err != nil {
		return File{}, err
	}
	location := resp.Header.Get("Location")
	resp.Body.Close()
	if location == "" {
		return File{}, fmt.Errorf("google drive did not return an upload session URL")
	}

	// The session URL carries its own credentials, so this PUT is a plain
	// request rather than an authClient call — and it must not be retried
	// blindly because the reader has already been consumed.
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, location, newProgressReader(r, p))
	if err != nil {
		return File{}, err
	}
	if size >= 0 {
		req.ContentLength = size
	}
	req.Header.Set("Content-Type", "application/octet-stream")

	var f gdriveFile
	if err := doJSON(g.cfg.HTTP, req, &f); err != nil {
		return File{}, err
	}
	return f.toFile(), nil
}

func (g *googleDrive) Mkdir(ctx context.Context, parentID, name string) (File, error) {
	body := map[string]any{
		"name":     name,
		"mimeType": gdriveFolder,
		"parents":  []string{gdriveParent(parentID)},
	}
	u := gdriveAPI + "/files?supportsAllDrives=true&fields=" + gdriveFields
	var f gdriveFile
	err := g.doJSON(ctx, func() (*http.Request, error) {
		return jsonRequest(ctx, http.MethodPost, u, body)
	}, &f)
	if err != nil {
		return File{}, err
	}
	return f.toFile(), nil
}

func (g *googleDrive) Rename(ctx context.Context, id, newName string) error {
	u := fmt.Sprintf("%s/files/%s?supportsAllDrives=true", gdriveAPI, url.PathEscape(id))
	return g.doJSON(ctx, func() (*http.Request, error) {
		return jsonRequest(ctx, http.MethodPatch, u, map[string]any{"name": newName})
	}, nil)
}

func (g *googleDrive) Delete(ctx context.Context, id string) error {
	// Trash rather than destroy: a mis-tap on a phone should be recoverable.
	// Note that trashed files still count against the account's quota until
	// the trash is emptied — see EmptyTrash and DeletePermanently.
	u := fmt.Sprintf("%s/files/%s?supportsAllDrives=true", gdriveAPI, url.PathEscape(id))
	return g.doJSON(ctx, func() (*http.Request, error) {
		return jsonRequest(ctx, http.MethodPatch, u, map[string]any{"trashed": true})
	}, nil)
}

// DeletePermanently destroys a file outright, bypassing the trash, so the
// space it occupied is reclaimed immediately.
func (g *googleDrive) DeletePermanently(ctx context.Context, id string) error {
	u := fmt.Sprintf("%s/files/%s?supportsAllDrives=true", gdriveAPI, url.PathEscape(id))
	return g.doJSON(ctx, func() (*http.Request, error) {
		return http.NewRequestWithContext(ctx, http.MethodDelete, u, nil)
	}, nil)
}

// EmptyTrash discards everything in the account's trash.
func (g *googleDrive) EmptyTrash(ctx context.Context) error {
	u := gdriveAPI + "/files/trash"
	return g.doJSON(ctx, func() (*http.Request, error) {
		return http.NewRequestWithContext(ctx, http.MethodDelete, u, nil)
	}, nil)
}

func (g *googleDrive) Quota(ctx context.Context) (Quota, error) {
	var res struct {
		StorageQuota struct {
			Limit string `json:"limit"`
			Usage string `json:"usage"`
		} `json:"storageQuota"`
		User struct {
			EmailAddress string `json:"emailAddress"`
		} `json:"user"`
	}
	u := gdriveAPI + "/about?fields=storageQuota,user"
	err := g.doJSON(ctx, func() (*http.Request, error) {
		return http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	}, &res)
	if err != nil {
		return Quota{}, err
	}
	var q Quota
	fmt.Sscanf(res.StorageQuota.Usage, "%d", &q.Used)
	fmt.Sscanf(res.StorageQuota.Limit, "%d", &q.Total)

	// Remember the signed-in identity so the UI can label the account.
	if res.User.EmailAddress != "" && g.cfg.Creds[CredAccountName] != res.User.EmailAddress {
		g.cfg.Creds[CredAccountName] = res.User.EmailAddress
		_ = g.cfg.Save(g.cfg.Creds)
	}
	return q, nil
}

// AccountName probes an OAuth account for a human label right after connect.
func AccountName(ctx context.Context, d Driver) string {
	type named interface{ accountName(context.Context) string }
	if n, ok := d.(named); ok {
		if s := n.accountName(ctx); s != "" {
			return s
		}
	}
	return ""
}

func (g *googleDrive) accountName(ctx context.Context) string {
	if _, err := g.Quota(ctx); err != nil {
		return ""
	}
	return strings.TrimSpace(g.cfg.Creds[CredAccountName])
}
