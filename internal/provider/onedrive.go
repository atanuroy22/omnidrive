package provider

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	graphAPI = "https://graph.microsoft.com/v1.0"
	// Graph requires every chunk except the last to be a multiple of 320 KiB.
	graphChunk = 16 * 320 * 1024 // 5 MiB
	// Below this size a plain PUT is allowed and is much faster.
	graphSimpleMax = 4 * 1024 * 1024
)

type oneDrive struct {
	*authClient
}

func newOneDrive(cfg Config) Driver {
	ep, _ := OAuthEndpoints(KindOneDrive)
	return &oneDrive{authClient: newAuthClient(cfg, ep)}
}

func (o *oneDrive) Kind() Kind { return KindOneDrive }

type graphItem struct {
	ID                   string `json:"id"`
	Name                 string `json:"name"`
	Size                 int64  `json:"size"`
	LastModifiedDateTime string `json:"lastModifiedDateTime"`
	Folder               *struct {
		ChildCount int `json:"childCount"`
	} `json:"folder"`
	File *struct {
		MimeType string `json:"mimeType"`
	} `json:"file"`
}

func (i graphItem) toFile() File {
	mod, _ := time.Parse(time.RFC3339, i.LastModifiedDateTime)
	f := File{ID: i.ID, Name: i.Name, Size: i.Size, Modified: mod, IsDir: i.Folder != nil}
	if i.File != nil {
		f.MIME = i.File.MimeType
	}
	return f
}

// itemPath builds the Graph URL segment for an item, handling the special
// spelling the root drive requires.
func itemPath(id string) string {
	if id == "" {
		return "/me/drive/root"
	}
	return "/me/drive/items/" + url.PathEscape(id)
}

func (o *oneDrive) List(ctx context.Context, folderID string) ([]File, error) {
	next := graphAPI + itemPath(folderID) + "/children?$top=500&$select=id,name,size,lastModifiedDateTime,folder,file"
	var out []File
	for next != "" {
		var res struct {
			Value    []graphItem `json:"value"`
			NextLink string      `json:"@odata.nextLink"`
		}
		u := next
		if err := o.doJSON(ctx, func() (*http.Request, error) {
			return http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		}, &res); err != nil {
			return nil, err
		}
		for _, it := range res.Value {
			out = append(out, it.toFile())
		}
		next = res.NextLink
	}
	SortFiles(out)
	return out, nil
}

func (o *oneDrive) Stat(ctx context.Context, id string) (File, error) {
	u := graphAPI + itemPath(id)
	var it graphItem
	if err := o.doJSON(ctx, func() (*http.Request, error) {
		return http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	}, &it); err != nil {
		return File{}, err
	}
	return it.toFile(), nil
}

func (o *oneDrive) Download(ctx context.Context, id string) (io.ReadCloser, int64, error) {
	u := graphAPI + itemPath(id) + "/content"
	resp, err := o.do(ctx, func() (*http.Request, error) {
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

// DownloadRange asks Graph for a byte window so playback can seek.
func (o *oneDrive) DownloadRange(ctx context.Context, id string, start, end int64) (io.ReadCloser, error) {
	u := graphAPI + itemPath(id) + "/content"
	resp, err := o.do(ctx, func() (*http.Request, error) {
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

func (o *oneDrive) Upload(ctx context.Context, parentID, name string, size int64, r io.Reader, p Progress) (File, error) {
	if size >= 0 && size <= graphSimpleMax {
		return o.uploadSimple(ctx, parentID, name, size, r, p)
	}
	return o.uploadSession(ctx, parentID, name, size, r, p)
}

func (o *oneDrive) uploadSimple(ctx context.Context, parentID, name string, size int64, r io.Reader, p Progress) (File, error) {
	body, err := io.ReadAll(r)
	if err != nil {
		return File{}, err
	}
	u := graphAPI + itemPath(parentID) + ":/" + url.PathEscape(name) + ":/content"
	var it graphItem
	err = o.doJSON(ctx, func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPut, u, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.ContentLength = int64(len(body))
		req.Header.Set("Content-Type", "application/octet-stream")
		return req, nil
	}, &it)
	if err != nil {
		return File{}, err
	}
	if p != nil {
		p(int64(len(body)))
	}
	return it.toFile(), nil
}

func (o *oneDrive) uploadSession(ctx context.Context, parentID, name string, size int64, r io.Reader, p Progress) (File, error) {
	if size < 0 {
		return File{}, fmt.Errorf("onedrive: upload requires a known content length")
	}
	createURL := graphAPI + itemPath(parentID) + ":/" + url.PathEscape(name) + ":/createUploadSession"
	var sess struct {
		UploadURL string `json:"uploadUrl"`
	}
	body := map[string]any{
		"item": map[string]any{"@microsoft.graph.conflictBehavior": "rename"},
	}
	if err := o.doJSON(ctx, func() (*http.Request, error) {
		return jsonRequest(ctx, http.MethodPost, createURL, body)
	}, &sess); err != nil {
		return File{}, err
	}
	if sess.UploadURL == "" {
		return File{}, fmt.Errorf("onedrive: no upload session URL returned")
	}

	buf := make([]byte, graphChunk)
	var sent int64
	for sent < size {
		n, readErr := io.ReadFull(r, buf)
		if n == 0 {
			if readErr != nil && readErr != io.EOF {
				return File{}, readErr
			}
			break
		}
		chunk := buf[:n]
		end := sent + int64(n) - 1

		req, err := http.NewRequestWithContext(ctx, http.MethodPut, sess.UploadURL, bytes.NewReader(chunk))
		if err != nil {
			return File{}, err
		}
		req.ContentLength = int64(n)
		req.Header.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", sent, end, size))

		resp, err := o.cfg.HTTP.Do(req)
		if err != nil {
			return File{}, err
		}
		// 202 means "chunk accepted, keep going"; 200/201 means the final
		// chunk landed and the body holds the created item.
		if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusCreated {
			var it graphItem
			if err := decodeAndClose(resp, &it); err != nil {
				return File{}, err
			}
			sent += int64(n)
			if p != nil {
				p(sent)
			}
			return it.toFile(), nil
		}
		if err := checkResponse(resp); err != nil {
			return File{}, err
		}
		resp.Body.Close()

		sent += int64(n)
		if p != nil {
			p(sent)
		}
		if readErr == io.EOF || readErr == io.ErrUnexpectedEOF {
			break
		}
	}
	return File{Name: name, Size: size}, nil
}

func (o *oneDrive) Mkdir(ctx context.Context, parentID, name string) (File, error) {
	u := graphAPI + itemPath(parentID) + "/children"
	body := map[string]any{
		"name":                              name,
		"folder":                            map[string]any{},
		"@microsoft.graph.conflictBehavior": "rename",
	}
	var it graphItem
	if err := o.doJSON(ctx, func() (*http.Request, error) {
		return jsonRequest(ctx, http.MethodPost, u, body)
	}, &it); err != nil {
		return File{}, err
	}
	return it.toFile(), nil
}

func (o *oneDrive) Rename(ctx context.Context, id, newName string) error {
	u := graphAPI + itemPath(id)
	return o.doJSON(ctx, func() (*http.Request, error) {
		return jsonRequest(ctx, http.MethodPatch, u, map[string]any{"name": newName})
	}, nil)
}

func (o *oneDrive) Delete(ctx context.Context, id string) error {
	u := graphAPI + itemPath(id)
	return o.doJSON(ctx, func() (*http.Request, error) {
		return http.NewRequestWithContext(ctx, http.MethodDelete, u, nil)
	}, nil)
}

// DeletePermanently skips the recycle bin. A plain Graph DELETE only moves the
// item there, where it goes on counting against the quota until someone visits
// onedrive.live.com, so this is what "delete" should really mean.
func (o *oneDrive) DeletePermanently(ctx context.Context, id string) error {
	if id == "" {
		return errors.New("refusing to permanently delete the drive root")
	}
	u := graphAPI + itemPath(id) + "/permanentDelete"
	return o.doJSON(ctx, func() (*http.Request, error) {
		return http.NewRequestWithContext(ctx, http.MethodPost, u, nil)
	}, nil)
}

func (o *oneDrive) Quota(ctx context.Context) (Quota, error) {
	var res struct {
		Quota struct {
			Used  int64 `json:"used"`
			Total int64 `json:"total"`
		} `json:"quota"`
		Owner struct {
			User struct {
				DisplayName string `json:"displayName"`
			} `json:"user"`
		} `json:"owner"`
	}
	u := graphAPI + "/me/drive"
	if err := o.doJSON(ctx, func() (*http.Request, error) {
		return http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	}, &res); err != nil {
		return Quota{}, err
	}
	if n := res.Owner.User.DisplayName; n != "" && o.cfg.Creds[CredAccountName] != n {
		o.cfg.Creds[CredAccountName] = n
		_ = o.cfg.Save(o.cfg.Creds)
	}
	return Quota{Used: res.Quota.Used, Total: res.Quota.Total}, nil
}

func (o *oneDrive) accountName(ctx context.Context) string {
	if _, err := o.Quota(ctx); err != nil {
		return ""
	}
	return strings.TrimSpace(o.cfg.Creds[CredAccountName])
}

func decodeAndClose(resp *http.Response, out any) error {
	defer resp.Body.Close()
	return jsonDecode(resp.Body, out)
}
