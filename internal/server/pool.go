package server

import (
	"context"
	"errors"
	"io"
	"net/http"
	"path"
	"time"

	"omnidrive/internal/alloc"
	"omnidrive/internal/pool"
	"omnidrive/internal/provider"
	"omnidrive/internal/store"
)

// The unified drive.
//
// Everything below makes the pool behave like an ordinary account so the rest
// of the API needs no special cases: the same /api/files, /api/upload and
// /api/transfer endpoints work with account=pool. Folder IDs in this mode are
// paths; file IDs stay real, so a pooled file downloads straight from the
// drive holding it.

// poolMembers returns the accounts that take part in the combined drive:
// cloud accounts only. Device storage stays separate.
func (s *Server) poolMembers() []*store.Account {
	var out []*store.Account
	for _, acc := range s.st.EnabledAccounts() {
		if acc.Kind.IsLocal() {
			continue
		}
		out = append(out, acc)
	}
	return out
}

// buildPool assembles the pool over every enabled cloud account.
func (s *Server) buildPool() (*pool.Pool, error) {
	var members []pool.Member
	for _, acc := range s.poolMembers() {
		drv, err := s.driver(acc)
		if err != nil {
			continue // a broken drive must not take the whole pool down
		}
		members = append(members, pool.Member{Account: acc, Driver: drv})
	}
	if len(members) == 0 {
		return nil, errors.New("no cloud drives connected yet")
	}
	pool.SortMembers(members)
	return pool.New(members), nil
}

// poolAccount describes the combined cloud drive the way the UI describes any
// other drive.
func (s *Server) poolAccount() map[string]any {
	var used, total int64
	var drives int
	for _, a := range s.poolMembers() {
		drives++
		used += a.QuotaUsed
		if a.QuotaTotal > 0 {
			total += a.QuotaTotal
		}
	}
	return map[string]any{
		"id": pool.ID, "kind": "pool", "label": pool.Label,
		"enabled": true, "quotaUsed": used, "quotaTotal": total,
		"drives": drives,
	}
}

// handlePoolList lists a folder in the combined namespace.
func (s *Server) handlePoolList(w http.ResponseWriter, r *http.Request, dir string) {
	p, err := s.buildPool()
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"account": s.poolAccount(), "folder": dir, "files": []provider.File{}, "pool": true,
		})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()

	files, err := p.List(ctx, dir)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	for i := range files {
		if !files[i].IsDir {
			files[i].Starred = s.st.IsStarred(files[i].AccountID, files[i].ID)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"account": s.poolAccount(),
		"folder":  dir,
		"files":   files,
		"pool":    true,
	})
}

// poolUploadTarget chooses the drive for a pooled upload and makes sure the
// destination folder exists on it. This is the heart of the "one big drive"
// behaviour: the caller names a path, not a drive.
func (s *Server) poolUploadTarget(ctx context.Context, dir string, size int64) (*store.Account, provider.Driver, string, error) {
	p, err := s.buildPool()
	if err != nil {
		return nil, nil, "", err
	}

	var chosen *store.Account
	err = s.st.Mutate(func(st *store.State) error {
		var enabled []*store.Account
		for _, a := range st.Accounts {
			// Cloud only: a file put "in the cloud" must not land on the phone.
			if a.Enabled && !a.Kind.IsLocal() {
				enabled = append(enabled, a)
			}
		}
		acc, cursor, aerr := alloc.Choose(enabled, st.Settings, size, st.RRCursor)
		if aerr != nil {
			return aerr
		}
		st.RRCursor = cursor
		chosen = acc
		return nil
	})
	if err != nil {
		return nil, nil, "", err
	}

	drv, err := s.driver(chosen)
	if err != nil {
		return nil, nil, "", err
	}
	folderID, err := p.EnsurePath(ctx, pool.Member{Account: chosen, Driver: drv}, dir)
	if err != nil {
		return nil, nil, "", err
	}
	return chosen, drv, folderID, nil
}

// handlePoolMkdir creates a folder in the pool. It is made on the drive the
// allocator would pick, which is enough: listings merge folders by name, so it
// appears once regardless of where it physically lives.
func (s *Server) handlePoolMkdir(w http.ResponseWriter, r *http.Request, parent, name string) {
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	acc, drv, parentID, err := s.poolUploadTarget(ctx, parent, 0)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if _, err := drv.Mkdir(ctx, parentID, name); err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, provider.File{
		ID: path.Join(parent, name), Name: name, IsDir: true,
		AccountID: pool.ID, AccountLabel: acc.Label, Path: path.Join(parent, name),
	})
}

// poolDeletePath removes a pooled folder from every drive that has it. A
// folder shown once must disappear once.
func (s *Server) poolDeletePath(ctx context.Context, dir string) (removed int, err error) {
	p, perr := s.buildPool()
	if perr != nil {
		return 0, perr
	}
	var failures []string
	for _, m := range p.Members() {
		id, ok, rerr := p.ResolveFor(ctx, m, dir)
		if rerr != nil || !ok {
			continue
		}
		if derr := deleteFunc(m.Driver, false)(ctx, id); derr != nil {
			failures = append(failures, m.Account.Label+": "+derr.Error())
			continue
		}
		removed++
	}
	if removed == 0 && len(failures) > 0 {
		return 0, errors.New(joinAll(failures))
	}
	return removed, nil
}

// poolRenamePath renames a pooled folder on every drive holding it, so the one
// entry the user sees changes once.
func (s *Server) poolRenamePath(ctx context.Context, dir, newName string) error {
	p, err := s.buildPool()
	if err != nil {
		return err
	}
	var renamed int
	var failures []string
	for _, m := range p.Members() {
		id, ok, rerr := p.ResolveFor(ctx, m, dir)
		if rerr != nil || !ok {
			continue
		}
		if rerr := m.Driver.Rename(ctx, id, newName); rerr != nil {
			failures = append(failures, m.Account.Label+": "+rerr.Error())
			continue
		}
		renamed++
	}
	if renamed == 0 {
		if len(failures) > 0 {
			return errors.New(joinAll(failures))
		}
		return errors.New("that folder was not found on any drive")
	}
	return nil
}

func joinAll(s []string) string {
	out := ""
	for i, v := range s {
		if i > 0 {
			out += "; "
		}
		out += v
	}
	return out
}

// handlePoolUpload streams a file into the pool, letting the allocator decide
// where it lands.
func (s *Server) handlePoolUpload(w http.ResponseWriter, r *http.Request, dir, name string, size int64, body io.Reader) {
	ctx := r.Context()
	acc, drv, folderID, err := s.poolUploadTarget(ctx, dir, size)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}

	job := s.jobs.start("upload", name, pool.Label, size)
	f, err := drv.Upload(ctx, folderID, name, size, body, func(sent int64) {
		s.jobs.progress(job, sent)
	})
	s.jobs.finish(job, err)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	f.AccountID, f.AccountLabel = acc.ID, acc.Label
	f.Path = path.Join(dir, name)

	writeJSON(w, http.StatusOK, map[string]any{
		"file": f, "job": job.ID,
		// Reported for transparency, but the UI has no need to show it.
		"storedOn": acc.Label,
	})
}
