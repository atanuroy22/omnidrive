package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"omnidrive/internal/alloc"
	"omnidrive/internal/pool"
	"omnidrive/internal/provider"
	"omnidrive/internal/store"
)

// handleFilesList returns the contents of a folder. With no account parameter
// it returns the virtual root: one entry per connected drive.
func (s *Server) handleFilesList(w http.ResponseWriter, r *http.Request) {
	accountID := r.URL.Query().Get("account")
	folderID := r.URL.Query().Get("folder")

	if accountID == "" {
		s.listVirtualRoot(w)
		return
	}
	// The combined drive: one namespace merged across every account.
	if accountID == pool.ID {
		s.handlePoolList(w, r, folderID)
		return
	}
	acc, drv, ok := s.account(w, accountID)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	files, err := drv.List(ctx, folderID)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	for i := range files {
		files[i].AccountID = acc.ID
		files[i].AccountLabel = acc.Label
		files[i].Starred = s.st.IsStarred(acc.ID, files[i].ID)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"account": acc.Public(),
		"folder":  folderID,
		"files":   files,
	})
}

// listVirtualRoot presents each drive as a top-level folder. Merging every
// provider into one namespace sounds appealing but produces duplicate names
// and ambiguous moves; showing the drives explicitly is honest and navigable.
func (s *Server) listVirtualRoot(w http.ResponseWriter) {
	accs := s.st.Accounts()
	files := make([]provider.File, 0, len(accs)+1)

	pooling := !s.st.Settings().PoolDisabled && len(s.poolMembers()) > 0

	// Device storage first: phone and SD card stay as themselves.
	for _, a := range accs {
		if a.Enabled && a.Kind.IsLocal() {
			files = append(files, provider.File{
				ID: "", Name: a.Label, IsDir: true,
				AccountID: a.ID, AccountLabel: a.Label, Modified: a.CreatedAt,
			})
		}
	}

	// Then the cloud: one combined entry when pooling is on, otherwise each
	// account on its own.
	if pooling {
		files = append(files, provider.File{
			ID: "", Name: pool.Label, IsDir: true,
			AccountID: pool.ID, AccountLabel: pool.Label,
		})
	} else {
		for _, a := range accs {
			if a.Enabled && !a.Kind.IsLocal() {
				files = append(files, provider.File{
					ID: "", Name: a.Label, IsDir: true,
					AccountID: a.ID, AccountLabel: a.Label, Modified: a.CreatedAt,
				})
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"account": nil,
		"folder":  "",
		"files":   files,
		"root":    true,
		"pooled":  s.poolAccount(),
	})
}

func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	_, drv, ok := s.account(w, q.Get("account"))
	if !ok {
		return
	}
	id := q.Get("id")
	if id == "" {
		writeErr(w, http.StatusBadRequest, errors.New("id is required"))
		return
	}

	rc, size, err := drv.Download(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	defer rc.Close()

	name := q.Get("name")
	if name == "" {
		name = "download"
	}
	if size > 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	}
	if ct := mime.TypeByExtension(filepath.Ext(name)); ct != "" {
		w.Header().Set("Content-Type", ct)
	} else {
		w.Header().Set("Content-Type", "application/octet-stream")
	}
	// RFC 5987 encoding so non-ASCII filenames survive the round trip.
	w.Header().Set("Content-Disposition",
		fmt.Sprintf(`attachment; filename="%s"; filename*=UTF-8''%s`,
			sanitizeASCII(name), url.PathEscape(name)))

	if _, err := io.Copy(w, rc); err != nil {
		// The response is already committed, so all we can do is log it.
		// A cancelled download from the browser lands here too.
		return
	}
}

func sanitizeASCII(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r < 0x20 || r > 0x7E || r == '"' || r == '\\' {
			b.WriteByte('_')
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func (s *Server) handleMkdir(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Account string `json:"account"`
		Parent  string `json:"parent"`
		Name    string `json:"name"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	if strings.TrimSpace(body.Name) == "" {
		writeErr(w, http.StatusBadRequest, errors.New("name is required"))
		return
	}
	if body.Account == pool.ID {
		s.handlePoolMkdir(w, r, body.Parent, strings.TrimSpace(body.Name))
		return
	}
	_, drv, ok := s.account(w, body.Account)
	if !ok {
		return
	}
	f, err := drv.Mkdir(r.Context(), body.Parent, strings.TrimSpace(body.Name))
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, f)
}

func (s *Server) handleRename(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Account string `json:"account"`
		ID      string `json:"id"`
		Name    string `json:"name"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	if strings.TrimSpace(body.Name) == "" {
		writeErr(w, http.StatusBadRequest, errors.New("name is required"))
		return
	}
	// A pooled folder exists on several drives at once and is addressed by
	// path, so it has to be renamed on each of them.
	if body.Account == pool.ID {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
		defer cancel()
		if err := s.poolRenamePath(ctx, body.ID, strings.TrimSpace(body.Name)); err != nil {
			writeErr(w, http.StatusBadGateway, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}
	_, drv, ok := s.account(w, body.Account)
	if !ok {
		return
	}
	if err := drv.Rename(r.Context(), body.ID, strings.TrimSpace(body.Name)); err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// deleteFunc picks what "delete" means for a drive.
//
// The two halves of this app want opposite defaults. On a cloud drive a
// recycle bin is a liability: the file goes on eating paid quota and the only
// way to reclaim it is to open the vendor's own website, which is exactly what
// this app exists to avoid. On the phone or an SD card there is no bin at all
// unless we provide one, and a mis-tap there is unrecoverable. So cloud deletes
// are final and local deletes are not.
//
// force is set when the caller has already decided — purging a single item out
// of the local bin.
func deleteFunc(drv provider.Driver, force bool) func(context.Context, string) error {
	hard, canPurge := drv.(provider.PermanentDeleter)
	if !canPurge {
		return drv.Delete
	}
	if force || !drv.Kind().IsLocal() {
		return hard.DeletePermanently
	}
	return drv.Delete
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Account   string   `json:"account"`
		IDs       []string `json:"ids"`
		Permanent bool     `json:"permanent"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	// Pooled folders live on several drives; removing the single entry the
	// user sees means removing it from each.
	if body.Account == pool.ID {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
		defer cancel()
		results := make([]map[string]any, 0, len(body.IDs))
		for _, dir := range body.IDs {
			entry := map[string]any{"id": dir, "ok": true}
			if _, err := s.poolDeletePath(ctx, dir); err != nil {
				entry["ok"], entry["error"] = false, err.Error()
			}
			results = append(results, entry)
		}
		writeJSON(w, http.StatusOK, map[string]any{"results": results})
		return
	}

	_, drv, ok := s.account(w, body.Account)
	if !ok {
		return
	}
	remove := deleteFunc(drv, body.Permanent)

	// Report per-item outcomes: a bulk delete where one item is already gone
	// should not look like a total failure.
	results := make([]map[string]any, 0, len(body.IDs))
	for _, id := range body.IDs {
		entry := map[string]any{"id": id, "ok": true}
		if err := remove(r.Context(), id); err != nil {
			entry["ok"] = false
			entry["error"] = err.Error()
		} else {
			_ = s.st.Star(body.Account, id, false)
		}
		results = append(results, entry)
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": results})
}

func (s *Server) handleStar(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Account string `json:"account"`
		ID      string `json:"id"`
		On      bool   `json:"on"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	if err := s.st.Star(body.Account, body.ID, body.On); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "starred": body.On})
}

// handleStarred resolves every starred key back into live file metadata,
// fanning out across accounts so one slow provider does not serialise the rest.
func (s *Server) handleStarred(w http.ResponseWriter, r *http.Request) {
	keys := s.st.StarredKeys()
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()

	type result struct {
		file provider.File
		ok   bool
	}
	results := make([]result, len(keys))
	var wg sync.WaitGroup

	for i, key := range keys {
		accountID, fileID, found := strings.Cut(key, "/")
		if !found {
			continue
		}
		acc, ok := s.st.Account(accountID)
		if !ok {
			continue
		}
		drv, err := s.driver(acc)
		if err != nil {
			continue
		}
		wg.Add(1)
		go func(i int, acc *store.Account, drv provider.Driver, fileID string) {
			defer wg.Done()
			f, err := drv.Stat(ctx, fileID)
			if err != nil {
				return
			}
			f.AccountID, f.AccountLabel, f.Starred = acc.ID, acc.Label, true
			results[i] = result{file: f, ok: true}
		}(i, acc, drv, fileID)
	}
	wg.Wait()

	files := make([]provider.File, 0, len(results))
	for _, r := range results {
		if r.ok {
			files = append(files, r.file)
		}
	}
	provider.SortFiles(files)
	writeJSON(w, http.StatusOK, map[string]any{"files": files})
}

// handleUpload streams a request body straight into a provider. The body is
// the raw file — not multipart — so nothing is buffered on the way through,
// which is what makes uploading a 4 GB video from a phone possible.
//
//	POST /api/upload?name=clip.mp4&account=<id>&folder=<id>&size=<bytes>
//
// Omitting account lets the configured allocation strategy pick the drive.
func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	name := strings.TrimSpace(q.Get("name"))
	if name == "" {
		writeErr(w, http.StatusBadRequest, errors.New("name is required"))
		return
	}
	// Reject path separators: a provider that interprets them would let a
	// crafted name write outside the intended folder.
	if strings.ContainsAny(name, `/\`) {
		writeErr(w, http.StatusBadRequest, errors.New("name must not contain path separators"))
		return
	}

	size := r.ContentLength
	if v := q.Get("size"); v != "" {
		if parsed, err := strconv.ParseInt(v, 10, 64); err == nil && parsed >= 0 {
			size = parsed
		}
	}

	// Uploading into the pool: the destination is a path, and the allocator
	// decides which drive actually receives it.
	if q.Get("account") == pool.ID {
		s.handlePoolUpload(w, r, q.Get("folder"), name, size, r.Body)
		return
	}

	acc, drv, err := s.uploadTarget(q.Get("account"), size)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}

	job := s.jobs.start("upload", name, acc.Label, size)
	// Uploads outlive the request only in the sense that we want them to
	// finish even if the browser navigates away mid-transfer; the body is tied
	// to the request, so we use the request context but do not cancel early.
	f, err := drv.Upload(r.Context(), q.Get("folder"), name, size, r.Body, func(sent int64) {
		s.jobs.progress(job, sent)
	})
	s.jobs.finish(job, err)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	f.AccountID, f.AccountLabel = acc.ID, acc.Label
	writeJSON(w, http.StatusOK, map[string]any{"file": f, "job": job.ID})
}

// uploadTarget resolves an explicit account, or applies the allocation
// strategy when the caller did not name one.
func (s *Server) uploadTarget(accountID string, size int64) (*store.Account, provider.Driver, error) {
	if accountID != "" {
		acc, ok := s.st.Account(accountID)
		if !ok {
			return nil, nil, fmt.Errorf("account %s not found", accountID)
		}
		drv, err := s.driver(acc)
		return acc, drv, err
	}

	var chosen *store.Account
	err := s.st.Mutate(func(st *store.State) error {
		var enabled []*store.Account
		for _, a := range st.Accounts {
			if a.Enabled {
				enabled = append(enabled, a)
			}
		}
		acc, cursor, err := alloc.Choose(enabled, st.Settings, size, st.RRCursor)
		if err != nil {
			return err
		}
		// Persisting the cursor inside the same transaction keeps round-robin
		// fair across restarts and concurrent uploads.
		st.RRCursor = cursor
		chosen = acc
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	drv, err := s.driver(chosen)
	return chosen, drv, err
}
