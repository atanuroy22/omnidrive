package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path"
	"strings"
	"time"

	"omnidrive/internal/pool"
	"omnidrive/internal/provider"
	"omnidrive/internal/store"
)

// Copying and moving between drives.
//
// Because every backend implements the same interface, a transfer is just a
// Download on one side piped into an Upload on the other — phone to cloud,
// cloud to cloud, cloud back to phone, all the same code. Nothing is buffered:
// a 4 GB video streams straight through.

type transferRequest struct {
	FromAccount string   `json:"fromAccount"`
	ToAccount   string   `json:"toAccount"`
	ToFolder    string   `json:"toFolder"`
	IDs         []string `json:"ids"`
	Move        bool     `json:"move"`
}

func (s *Server) handleTransfer(w http.ResponseWriter, r *http.Request) {
	var body transferRequest
	if !decodeBody(w, r, &body) {
		return
	}
	if len(body.IDs) == 0 {
		writeErr(w, http.StatusBadRequest, errors.New("nothing selected"))
		return
	}
	src, srcDrv, ok := s.account(w, body.FromAccount)
	if !ok {
		return
	}

	// Copying into the combined drive: the destination is a path, and each
	// file is placed on whichever account has room.
	if body.ToAccount == pool.ID {
		s.transferIntoPool(w, r, src, srcDrv, body)
		return
	}

	dst, dstDrv, ok := s.account(w, body.ToAccount)
	if !ok {
		return
	}
	// Same-drive transfers are allowed — that is folder-to-folder on one
	// drive, the most ordinary operation there is. What must be refused is a
	// folder into itself or into its own descendant, which would copy forever.
	if src.ID == dst.ID {
		for _, id := range body.IDs {
			if withinTarget(body.ToFolder, id) {
				writeErr(w, http.StatusBadRequest,
					errors.New("that would copy a folder into itself"))
				return
			}
		}
	}

	// The browser may navigate away mid-copy, so run detached from the request
	// and report progress over the event stream instead.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 6*time.Hour)

	verb := "copy"
	if body.Move {
		verb = "move"
	}
	job := s.jobs.start(verb, fmt.Sprintf("%d item(s) → %s", len(body.IDs), dst.Label), dst.Label, 0)

	go func() {
		defer cancel()
		var failed int
		for _, id := range body.IDs {
			if err := s.transferOne(ctx, srcDrv, dstDrv, src, dst, id, body.ToFolder, body.Move, job); err != nil {
				failed++
				s.jobs.note(job, fmt.Sprintf("%s: %v", id, err))
			}
		}
		if failed > 0 {
			s.jobs.finish(job, fmt.Errorf("%d of %d item(s) failed", failed, len(body.IDs)))
			return
		}
		s.jobs.finish(job, nil)
	}()

	writeJSON(w, http.StatusAccepted, map[string]any{
		"ok": true, "job": job.ID, "items": len(body.IDs),
		"from": src.Label, "to": dst.Label, "move": body.Move,
	})
}

// transferIntoPool copies a selection into the combined cloud drive.
//
// The destination is a folder *path*, not a drive: every file is placed
// individually by the allocator, so a batch too large for any one account
// still succeeds by spreading across several.
func (s *Server) transferIntoPool(w http.ResponseWriter, r *http.Request,
	src *store.Account, srcDrv provider.Driver, body transferRequest) {

	if _, err := s.buildPool(); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 6*time.Hour)

	verb := "copy"
	if body.Move {
		verb = "move"
	}
	job := s.jobs.start(verb, fmt.Sprintf("%d item(s) → %s", len(body.IDs), pool.Label), pool.Label, 0)

	go func() {
		defer cancel()
		var failed int
		for _, id := range body.IDs {
			if err := s.poolTransferOne(ctx, srcDrv, src, id, body.ToFolder, body.Move, job, 0); err != nil {
				failed++
				s.jobs.note(job, fmt.Sprintf("%s: %v", id, err))
			}
		}
		if failed > 0 {
			s.jobs.finish(job, fmt.Errorf("%d of %d item(s) failed", failed, len(body.IDs)))
			return
		}
		s.jobs.finish(job, nil)
	}()

	writeJSON(w, http.StatusAccepted, map[string]any{
		"ok": true, "job": job.ID, "items": len(body.IDs),
		"from": src.Label, "to": pool.Label, "move": body.Move,
	})
}

func (s *Server) poolTransferOne(ctx context.Context, srcDrv provider.Driver,
	src *store.Account, id, destDir string, move bool, job *Job, depth int) error {

	if depth > maxTransferDepth {
		return fmt.Errorf("folder nesting deeper than %d levels", maxTransferDepth)
	}
	meta, err := srcDrv.Stat(ctx, id)
	if err != nil {
		return fmt.Errorf("read source: %w", err)
	}

	if meta.IsDir {
		children, err := srcDrv.List(ctx, meta.ID)
		if err != nil {
			return fmt.Errorf("list %q: %w", meta.Name, err)
		}
		sub := path.Join(destDir, meta.Name)
		for _, child := range children {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := s.poolTransferOne(ctx, srcDrv, src, child.ID, sub, move, job, depth+1); err != nil {
				return err
			}
		}
		if move {
			if err := srcDrv.Delete(ctx, meta.ID); err != nil {
				return fmt.Errorf("copied, but could not remove the original folder: %w", err)
			}
		}
		return nil
	}

	// Each file gets its own allocation decision, and the folder path is
	// created on whichever drive receives it.
	_, dstDrv, folderID, err := s.poolUploadTarget(ctx, destDir, meta.Size)
	if err != nil {
		return err
	}
	if err := s.copyFile(ctx, srcDrv, dstDrv, meta, folderID, job); err != nil {
		return err
	}
	if move {
		if err := srcDrv.Delete(ctx, id); err != nil {
			return fmt.Errorf("copied, but could not remove the original: %w", err)
		}
		_ = s.st.Star(src.ID, id, false)
	}
	return nil
}

// withinTarget reports whether dest is the item itself or sits inside it.
//
// Only meaningful for the backends whose IDs are paths — local, WebDAV, S3,
// the pool. Providers with opaque IDs (Google Drive, OneDrive) still get the
// equality check, which catches the common case of picking the same folder.
func withinTarget(dest, item string) bool {
	dest = strings.Trim(strings.ReplaceAll(dest, "\\", "/"), "/")
	item = strings.Trim(strings.ReplaceAll(item, "\\", "/"), "/")
	if item == "" {
		return false
	}
	return dest == item || strings.HasPrefix(dest, item+"/")
}

// transferOne copies a single entry, recursing into folders.
func (s *Server) transferOne(ctx context.Context, srcDrv, dstDrv provider.Driver,
	src, dst *store.Account, id, destFolder string, move bool, job *Job) error {

	meta, err := srcDrv.Stat(ctx, id)
	if err != nil {
		return fmt.Errorf("read source: %w", err)
	}

	if meta.IsDir {
		return s.transferFolder(ctx, srcDrv, dstDrv, src, dst, meta, destFolder, move, job, 0)
	}
	if err := s.copyFile(ctx, srcDrv, dstDrv, meta, destFolder, job); err != nil {
		return err
	}
	if move {
		// Only remove the source once the copy has actually landed.
		if err := srcDrv.Delete(ctx, id); err != nil {
			return fmt.Errorf("copied, but could not remove the original: %w", err)
		}
		_ = s.st.Star(src.ID, id, false)
	}
	return nil
}

// maxTransferDepth stops a symlink loop or a pathological tree from running
// forever on a phone.
const maxTransferDepth = 32

func (s *Server) transferFolder(ctx context.Context, srcDrv, dstDrv provider.Driver,
	src, dst *store.Account, dir provider.File, destParent string, move bool, job *Job, depth int) error {

	if depth > maxTransferDepth {
		return fmt.Errorf("folder nesting deeper than %d levels", maxTransferDepth)
	}
	created, err := dstDrv.Mkdir(ctx, destParent, dir.Name)
	if err != nil {
		return fmt.Errorf("create %q: %w", dir.Name, err)
	}
	children, err := srcDrv.List(ctx, dir.ID)
	if err != nil {
		return fmt.Errorf("list %q: %w", dir.Name, err)
	}
	for _, child := range children {
		if err := ctx.Err(); err != nil {
			return err
		}
		if child.IsDir {
			err = s.transferFolder(ctx, srcDrv, dstDrv, src, dst, child, created.ID, move, job, depth+1)
		} else {
			err = s.copyFile(ctx, srcDrv, dstDrv, child, created.ID, job)
		}
		if err != nil {
			return err
		}
	}
	if move {
		if err := srcDrv.Delete(ctx, dir.ID); err != nil {
			return fmt.Errorf("copied, but could not remove the original folder: %w", err)
		}
	}
	return nil
}

func (s *Server) copyFile(ctx context.Context, srcDrv, dstDrv provider.Driver,
	meta provider.File, destFolder string, job *Job) error {

	rc, size, err := srcDrv.Download(ctx, meta.ID)
	if err != nil {
		return fmt.Errorf("download: %w", err)
	}
	defer rc.Close()

	if size <= 0 {
		size = meta.Size
	}
	name := meta.Name
	if name == "" {
		name = path.Base(meta.ID)
	}
	// Some backends refuse an unknown length; the metadata size is the best
	// answer available and is what every provider here reports.
	s.jobs.retarget(job, name, size)

	base := job.Sent
	_, err = dstDrv.Upload(ctx, destFolder, name, size, rc, func(sent int64) {
		s.jobs.progress(job, base+sent)
	})
	if err != nil {
		return fmt.Errorf("upload: %w", err)
	}
	s.jobs.progress(job, base+size)
	return nil
}

// handleTransferTargets lists the places a selection could be sent, which is
// every enabled account other than the one being copied from.
func (s *Server) handleTransferTargets(w http.ResponseWriter, r *http.Request) {
	pooling := !s.st.Settings().PoolDisabled && len(s.poolMembers()) > 0

	// Every drive is a destination, including the one being copied from.
	// Excluding the source made the commonest operation of all impossible:
	// moving a file from one folder to another on the same drive. The picker
	// chooses the folder, so same-drive transfers are perfectly well defined.
	var out []map[string]any

	// Device storage first, individually.
	for _, a := range s.st.EnabledAccounts() {
		if !a.Kind.IsLocal() {
			continue
		}
		m := a.Public()
		m["isLocal"] = true
		out = append(out, m)
	}

	// The cloud is offered the same way it is browsed: as one destination when
	// combining is on, and account by account when it is off.
	if pooling {
		m := s.poolAccount()
		m["isLocal"] = false
		out = append(out, m)
	} else {
		for _, a := range s.st.EnabledAccounts() {
			if a.Kind.IsLocal() {
				continue
			}
			m := a.Public()
			m["isLocal"] = false
			out = append(out, m)
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// handleSearch looks for a name across one drive or all of them. Providers
// differ wildly in search support, so this walks folders directly: predictable
// everywhere, and the only option for the ones with no search API at all.
func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	if q == "" {
		writeErr(w, http.StatusBadRequest, errors.New("a search term is required"))
		return
	}
	accountID := r.URL.Query().Get("account")
	folder := r.URL.Query().Get("folder")

	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	var accounts []*store.Account
	if accountID != "" {
		acc, ok := s.st.Account(accountID)
		if !ok {
			writeErr(w, http.StatusNotFound, fmt.Errorf("account %s not found", accountID))
			return
		}
		accounts = []*store.Account{acc}
	} else {
		accounts = s.st.EnabledAccounts()
		folder = "" // a cross-drive search always starts at each root
	}

	const maxResults = 300
	var found []provider.File

	for _, acc := range accounts {
		drv, err := s.driver(acc)
		if err != nil {
			continue
		}
		s.searchWalk(ctx, drv, acc, folder, q, &found, maxResults, 0)
		if len(found) >= maxResults {
			break
		}
	}
	provider.SortFiles(found)
	writeJSON(w, http.StatusOK, map[string]any{
		"files": found, "query": q, "truncated": len(found) >= maxResults,
	})
}

// searchWalk is a bounded breadth-limited descent; a full crawl of a large
// Drive over mobile data would take minutes and burn the user's allowance.
func (s *Server) searchWalk(ctx context.Context, drv provider.Driver, acc *store.Account,
	folder, needle string, found *[]provider.File, limit, depth int) {

	if depth > 6 || len(*found) >= limit || ctx.Err() != nil {
		return
	}
	entries, err := drv.List(ctx, folder)
	if err != nil {
		return
	}
	var subdirs []provider.File
	for _, e := range entries {
		if strings.Contains(strings.ToLower(e.Name), needle) {
			e.AccountID, e.AccountLabel = acc.ID, acc.Label
			e.Starred = s.st.IsStarred(acc.ID, e.ID)
			*found = append(*found, e)
			if len(*found) >= limit {
				return
			}
		}
		if e.IsDir {
			subdirs = append(subdirs, e)
		}
	}
	for _, d := range subdirs {
		s.searchWalk(ctx, drv, acc, d.ID, needle, found, limit, depth+1)
		if len(*found) >= limit {
			return
		}
	}
}
