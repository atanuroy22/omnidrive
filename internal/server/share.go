package server

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"omnidrive/internal/pool"
	"omnidrive/internal/provider"
	"omnidrive/internal/store"
)

// handleShareLink mints a public URL for one file, or revokes it.
//
// The link comes from the provider rather than from this server. OmniDrive
// listens on the phone's loopback address, so a URL it served itself would be
// useless to anyone else and would die when the phone slept; a Google or
// Dropbox link is served by their CDN and works from any network with the phone
// switched off.
func (s *Server) handleShareLink(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Account string `json:"account"`
		ID      string `json:"id"`
		Name    string `json:"name"`
		IsDir   bool   `json:"isDir"`
		Revoke  bool   `json:"revoke"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	if body.ID == "" {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("a file is required"))
		return
	}
	if body.Account == pool.ID {
		writeErr(w, http.StatusBadRequest, fmt.Errorf(
			"pick the file from its own drive to share it — a combined folder has no single link"))
		return
	}

	acc, drv, ok := s.account(w, body.Account)
	if !ok {
		return
	}
	sharer, supported := drv.(provider.Sharer)
	if !supported {
		writeErr(w, http.StatusBadRequest, fmt.Errorf(
			"%s cannot publish a link. Move the file to a cloud drive first — "+
				"a link to phone storage would only work on this device", acc.Label))
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	if body.Revoke {
		if err := sharer.Unshare(ctx, body.ID); err != nil {
			writeErr(w, http.StatusBadGateway, err)
			return
		}
		_ = s.st.RemoveShare(acc.ID, body.ID)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "revoked": true})
		return
	}

	link, err := sharer.ShareLink(ctx, body.ID)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}

	// Remember what was published. Without a register the user has no way to
	// find a link again, and a link you cannot find is one you cannot revoke.
	name, isDir := body.Name, body.IsDir
	if name == "" {
		if f, ferr := drv.Stat(ctx, body.ID); ferr == nil {
			name, isDir = f.Name, f.IsDir
		} else {
			name = body.ID
		}
	}
	_ = s.st.AddShare(store.SharedLink{
		AccountID: acc.ID,
		FileID:    body.ID,
		Name:      name,
		URL:       link.URL,
		Direct:    link.Direct,
		IsDir:     isDir,
	})

	writeJSON(w, http.StatusOK, map[string]any{
		"url":     link.URL,
		"direct":  link.Direct,
		"expires": link.Expires,
		"drive":   acc.Label,
		"name":    name,
	})
}

// handleSharesList reports every link this device has published, so they can be
// reviewed and taken back in one place.
func (s *Server) handleSharesList(w http.ResponseWriter, r *http.Request) {
	type row struct {
		store.SharedLink
		Drive string `json:"drive"`
		Kind  string `json:"kind"`
	}
	links := s.st.Shares()
	out := make([]row, 0, len(links))
	for _, l := range links {
		e := row{SharedLink: l, Drive: l.AccountID}
		if acc, ok := s.st.Account(l.AccountID); ok {
			e.Drive, e.Kind = acc.Label, string(acc.Kind)
		}
		out = append(out, e)
	}
	writeJSON(w, http.StatusOK, map[string]any{"links": out})
}
