package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path"
	"strings"
	"time"
)

const (
	dropboxRPC     = "https://api.dropboxapi.com"
	dropboxContent = "https://content.dropboxapi.com"
	dropboxChunk   = 8 << 20 // 8 MiB session chunks
	dropboxSimple  = 100 << 20
)

type dropbox struct {
	*authClient
}

func newDropbox(cfg Config) Driver {
	ep, _ := OAuthEndpoints(KindDropbox)
	return &dropbox{authClient: newAuthClient(cfg, ep)}
}

func (d *dropbox) Kind() Kind { return KindDropbox }

type dbxEntry struct {
	Tag            string `json:".tag"`
	ID             string `json:"id"`
	Name           string `json:"name"`
	PathDisplay    string `json:"path_display"`
	PathLower      string `json:"path_lower"`
	Size           int64  `json:"size"`
	ServerModified string `json:"server_modified"`
}

func (e dbxEntry) toFile() File {
	mod, _ := time.Parse(time.RFC3339, e.ServerModified)
	return File{
		ID:       e.ID,
		Name:     e.Name,
		IsDir:    e.Tag == "folder",
		Size:     e.Size,
		Modified: mod,
		Path:     e.PathDisplay,
	}
}

// dbxPath converts our opaque ID into something the API accepts. Dropbox
// understands "id:..." wherever it takes a path, and the empty string is root.
func dbxPath(id string) string {
	if id == "" || id == "/" {
		return ""
	}
	return id
}

func (d *dropbox) rpc(ctx context.Context, endpoint string, arg any, out any) error {
	u := dropboxRPC + endpoint
	return d.doJSON(ctx, func() (*http.Request, error) {
		var body io.Reader = strings.NewReader("null")
		if arg != nil {
			b, err := json.Marshal(arg)
			if err != nil {
				return nil, err
			}
			body = bytes.NewReader(b)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, body)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		return req, nil
	}, out)
}

func (d *dropbox) List(ctx context.Context, folderID string) ([]File, error) {
	var res struct {
		Entries []dbxEntry `json:"entries"`
		Cursor  string     `json:"cursor"`
		HasMore bool       `json:"has_more"`
	}
	arg := map[string]any{"path": dbxPath(folderID), "limit": 1000}
	if err := d.rpc(ctx, "/2/files/list_folder", arg, &res); err != nil {
		return nil, err
	}
	var out []File
	for _, e := range res.Entries {
		out = append(out, e.toFile())
	}
	for res.HasMore {
		cur := res.Cursor
		res.Entries, res.HasMore, res.Cursor = nil, false, ""
		if err := d.rpc(ctx, "/2/files/list_folder/continue", map[string]any{"cursor": cur}, &res); err != nil {
			return nil, err
		}
		for _, e := range res.Entries {
			out = append(out, e.toFile())
		}
	}
	SortFiles(out)
	return out, nil
}

func (d *dropbox) Stat(ctx context.Context, id string) (File, error) {
	var e dbxEntry
	if err := d.rpc(ctx, "/2/files/get_metadata", map[string]any{"path": dbxPath(id)}, &e); err != nil {
		return File{}, err
	}
	return e.toFile(), nil
}

func (d *dropbox) Download(ctx context.Context, id string) (io.ReadCloser, int64, error) {
	arg, err := asciiJSON(map[string]any{"path": dbxPath(id)})
	if err != nil {
		return nil, 0, err
	}
	resp, err := d.do(ctx, func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, dropboxContent+"/2/files/download", nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Dropbox-API-Arg", arg)
		return req, nil
	})
	if err != nil {
		return nil, 0, err
	}
	if err := checkResponse(resp); err != nil {
		return nil, 0, err
	}
	return resp.Body, resp.ContentLength, nil
}

// DownloadRange asks Dropbox for a byte window; its content endpoints honour
// Range even though the argument itself travels in a header.
func (d *dropbox) DownloadRange(ctx context.Context, id string, start, end int64) (io.ReadCloser, error) {
	arg, err := asciiJSON(map[string]any{"path": dbxPath(id)})
	if err != nil {
		return nil, err
	}
	resp, err := d.do(ctx, func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, dropboxContent+"/2/files/download", nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Dropbox-API-Arg", arg)
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

func (d *dropbox) Upload(ctx context.Context, parentID, name string, size int64, r io.Reader, p Progress) (File, error) {
	dest, err := d.childPath(ctx, parentID, name)
	if err != nil {
		return File{}, err
	}
	if size >= 0 && size <= dropboxSimple {
		return d.uploadSimple(ctx, dest, r, p)
	}
	return d.uploadSession(ctx, dest, r, p)
}

func (d *dropbox) uploadSimple(ctx context.Context, dest string, r io.Reader, p Progress) (File, error) {
	body, err := io.ReadAll(r)
	if err != nil {
		return File{}, err
	}
	arg, err := asciiJSON(map[string]any{"path": dest, "mode": "add", "autorename": true, "mute": true})
	if err != nil {
		return File{}, err
	}
	var e dbxEntry
	err = d.doJSON(ctx, func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, dropboxContent+"/2/files/upload", bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.ContentLength = int64(len(body))
		req.Header.Set("Dropbox-API-Arg", arg)
		req.Header.Set("Content-Type", "application/octet-stream")
		return req, nil
	}, &e)
	if err != nil {
		return File{}, err
	}
	if p != nil {
		p(int64(len(body)))
	}
	return e.toFile(), nil
}

func (d *dropbox) uploadSession(ctx context.Context, dest string, r io.Reader, p Progress) (File, error) {
	buf := make([]byte, dropboxChunk)
	var sessionID string
	var offset int64

	for {
		n, readErr := io.ReadFull(r, buf)
		last := readErr == io.EOF || readErr == io.ErrUnexpectedEOF
		if readErr != nil && !last {
			return File{}, readErr
		}
		chunk := buf[:n]

		switch {
		case sessionID == "":
			var res struct {
				SessionID string `json:"session_id"`
			}
			if err := d.contentPost(ctx, "/2/files/upload_session/start",
				map[string]any{"close": false}, chunk, &res); err != nil {
				return File{}, err
			}
			sessionID = res.SessionID
		case !last:
			arg := map[string]any{
				"cursor": map[string]any{"session_id": sessionID, "offset": offset},
			}
			if err := d.contentPost(ctx, "/2/files/upload_session/append_v2", arg, chunk, nil); err != nil {
				return File{}, err
			}
		}
		offset += int64(n)
		if p != nil {
			p(offset)
		}
		if last {
			arg := map[string]any{
				"cursor": map[string]any{"session_id": sessionID, "offset": offset - int64(n)},
				"commit": map[string]any{"path": dest, "mode": "add", "autorename": true, "mute": true},
			}
			// The final chunk is committed together with the metadata, except
			// when it was already consumed by session/start above.
			payload := chunk
			if offset == int64(n) {
				arg["cursor"] = map[string]any{"session_id": sessionID, "offset": offset}
				payload = nil
			}
			var e dbxEntry
			if err := d.contentPost(ctx, "/2/files/upload_session/finish", arg, payload, &e); err != nil {
				return File{}, err
			}
			return e.toFile(), nil
		}
	}
}

func (d *dropbox) contentPost(ctx context.Context, endpoint string, arg any, body []byte, out any) error {
	encoded, err := asciiJSON(arg)
	if err != nil {
		return err
	}
	return d.doJSON(ctx, func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, dropboxContent+endpoint, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.ContentLength = int64(len(body))
		req.Header.Set("Dropbox-API-Arg", encoded)
		req.Header.Set("Content-Type", "application/octet-stream")
		return req, nil
	}, out)
}

// childPath resolves a parent ID to a real path so we can name the new object.
func (d *dropbox) childPath(ctx context.Context, parentID, name string) (string, error) {
	if dbxPath(parentID) == "" {
		return "/" + name, nil
	}
	parent, err := d.Stat(ctx, parentID)
	if err != nil {
		return "", err
	}
	if parent.Path == "" {
		return "/" + name, nil
	}
	return path.Join(parent.Path, name), nil
}

func (d *dropbox) Mkdir(ctx context.Context, parentID, name string) (File, error) {
	dest, err := d.childPath(ctx, parentID, name)
	if err != nil {
		return File{}, err
	}
	var res struct {
		Metadata dbxEntry `json:"metadata"`
	}
	if err := d.rpc(ctx, "/2/files/create_folder_v2",
		map[string]any{"path": dest, "autorename": true}, &res); err != nil {
		return File{}, err
	}
	f := res.Metadata.toFile()
	f.IsDir = true
	return f, nil
}

func (d *dropbox) Rename(ctx context.Context, id, newName string) error {
	cur, err := d.Stat(ctx, id)
	if err != nil {
		return err
	}
	if cur.Path == "" {
		return fmt.Errorf("dropbox: cannot rename the root folder")
	}
	dest := path.Join(path.Dir(cur.Path), newName)
	return d.rpc(ctx, "/2/files/move_v2", map[string]any{
		"from_path": cur.Path, "to_path": dest, "autorename": false,
	}, nil)
}

func (d *dropbox) Delete(ctx context.Context, id string) error {
	return d.rpc(ctx, "/2/files/delete_v2", map[string]any{"path": dbxPath(id)}, nil)
}

func (d *dropbox) Quota(ctx context.Context) (Quota, error) {
	var res struct {
		Used       int64 `json:"used"`
		Allocation struct {
			Allocated int64 `json:"allocated"`
		} `json:"allocation"`
	}
	if err := d.rpc(ctx, "/2/users/get_space_usage", nil, &res); err != nil {
		return Quota{}, err
	}
	return Quota{Used: res.Used, Total: res.Allocation.Allocated}, nil
}

func (d *dropbox) accountName(ctx context.Context) string {
	var res struct {
		Email string `json:"email"`
		Name  struct {
			DisplayName string `json:"display_name"`
		} `json:"name"`
	}
	if err := d.rpc(ctx, "/2/users/get_current_account", nil, &res); err != nil {
		return ""
	}
	if res.Email != "" {
		return res.Email
	}
	return res.Name.DisplayName
}

// asciiJSON encodes v as JSON with every non-ASCII rune escaped. Dropbox
// passes arguments in an HTTP header, and a header carrying raw UTF-8 (an
// emoji in a filename, say) is rejected outright.
func asciiJSON(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	var sb strings.Builder
	for _, r := range string(b) {
		if r < 0x80 {
			sb.WriteRune(r)
			continue
		}
		if r > 0xFFFF {
			r -= 0x10000
			fmt.Fprintf(&sb, "\\u%04x\\u%04x", 0xD800+(r>>10), 0xDC00+(r&0x3FF))
			continue
		}
		fmt.Fprintf(&sb, "\\u%04x", r)
	}
	return sb.String(), nil
}
