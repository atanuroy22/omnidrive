package server

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"omnidrive/internal/provider"
	"omnidrive/internal/store"
)

// The recycle bin endpoints only apply to drives that keep one we can reach.
// In practice that is phone and SD card storage, where OmniDrive runs the bin
// itself — deleting from a cloud drive is final, because a file sitting in a
// vendor's bin goes on consuming the quota you are paying for.

// trashDriver resolves the account and asserts it can be browsed as a bin.
func (s *Server) trashDriver(w http.ResponseWriter, r *http.Request) (*store.Account, provider.TrashBrowser, bool) {
	acc, drv, ok := s.account(w, r.PathValue("id"))
	if !ok {
		return nil, nil, false
	}
	bin, supported := drv.(provider.TrashBrowser)
	if !supported {
		writeErr(w, http.StatusBadRequest, fmt.Errorf(
			"%s has no recycle bin that can be opened from here", acc.Label))
		return nil, nil, false
	}
	return acc, bin, true
}

func (s *Server) handleTrashList(w http.ResponseWriter, r *http.Request) {
	acc, bin, ok := s.trashDriver(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	items, err := bin.ListTrash(ctx)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	var total int64
	for _, it := range items {
		total += it.Size
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"drive": acc.Label,
		"items": items,
		"size":  total,
	})
}

// handleTrashRestore puts items back where they were deleted from.
func (s *Server) handleTrashRestore(w http.ResponseWriter, r *http.Request) {
	_, bin, ok := s.trashDriver(w, r)
	if !ok {
		return
	}
	var body struct {
		IDs []string `json:"ids"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()

	// Per-item results: restoring ten photos where one folder has since been
	// deleted should put back the other nine, not abort.
	results := make([]map[string]any, 0, len(body.IDs))
	for _, id := range body.IDs {
		entry := map[string]any{"id": id, "ok": true}
		if err := bin.RestoreTrash(ctx, id); err != nil {
			entry["ok"], entry["error"] = false, err.Error()
		}
		results = append(results, entry)
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": results})
}

// handleTrashPurge destroys individual items in the bin, leaving the rest.
func (s *Server) handleTrashPurge(w http.ResponseWriter, r *http.Request) {
	acc, drv, ok := s.account(w, r.PathValue("id"))
	if !ok {
		return
	}
	hard, supported := drv.(provider.PermanentDeleter)
	if !supported {
		writeErr(w, http.StatusBadRequest, fmt.Errorf(
			"%s cannot delete individual items permanently", acc.Label))
		return
	}
	var body struct {
		IDs []string `json:"ids"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()

	results := make([]map[string]any, 0, len(body.IDs))
	for _, id := range body.IDs {
		entry := map[string]any{"id": id, "ok": true}
		if err := hard.DeletePermanently(ctx, id); err != nil {
			entry["ok"], entry["error"] = false, err.Error()
		}
		results = append(results, entry)
	}
	if q, qerr := drv.Quota(ctx); qerr == nil {
		_ = s.st.SetQuota(acc.ID, q)
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": results})
}
