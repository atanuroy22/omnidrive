package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"omnidrive/internal/portable"
	"omnidrive/internal/store"
)

// --- file transport ---

func (s *Server) handleExport(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Passphrase string `json:"passphrase"`
	}
	// GET is supported so the download can be handled as an ordinary file
	// fetch. Building it in the page as a blob: URL instead means the Android
	// downloader is handed a URL it cannot re-request.
	//
	// No passphrase over GET: it would end up in the URL, and from there in
	// logs and error messages.
	if r.Method == http.MethodPost {
		if !decodeBody(w, r, &body) {
			return
		}
	}

	blob, err := portable.Export(s.st, body.Passphrase)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.Itoa(len(blob)))
	w.Header().Set("Content-Disposition", `attachment; filename="`+portable.BundleName+`"`)
	_, _ = w.Write(blob)
}

func (s *Server) handleImport(w http.ResponseWriter, r *http.Request) {
	// The bundle arrives as multipart because it comes from a file picker.
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("could not read upload: %w", err))
		return
	}
	file, _, err := r.FormFile("bundle")
	if err != nil {
		writeErr(w, http.StatusBadRequest, errors.New("no bundle file was supplied"))
		return
	}
	defer file.Close()

	blob, err := io.ReadAll(io.LimitReader(file, 32<<20))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	replace := r.FormValue("replace") == "true"
	s.applyBundle(w, blob, r.FormValue("passphrase"), replace)
}

// applyBundle decrypts, merges and reports — shared by every transport.
func (s *Server) applyBundle(w http.ResponseWriter, blob []byte, passphrase string, replace bool) {
	summary, added, updated, err := portable.Import(s.st, blob, passphrase, replace)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	// Credentials changed underneath any cached driver, so drop them all.
	s.invalidateDrivers()

	// Freshly imported accounts have stale quota figures from the other
	// device; refresh in the background so the UI settles quickly.
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		s.RefreshQuotas(ctx)
	}()

	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "added": added, "updated": updated, "bundle": summary,
	})
}

// --- LAN pairing ---

// handlePairStart publishes the current setup for another device to collect.
//
// The offer is served from a separate listener bound to all interfaces, while
// the main API stays on loopback. That way turning on pairing exposes exactly
// one endpoint — protected by a one-time code, single use, ten-minute life —
// and nothing else.
func (s *Server) handlePairStart(w http.ResponseWriter, r *http.Request) {
	port, err := s.ensurePairListener()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	offer, err := s.pairer.Create(s.st, port)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"link":      offer.Link,
		"url":       offer.URL,
		"code":      offer.Code,
		"expiresAt": offer.ExpiresAt,
		"accounts":  offer.Accounts,
	})
}

func (s *Server) handlePairJoin(w http.ResponseWriter, r *http.Request) {
	var body struct {
		URL     string `json:"url"`
		Code    string `json:"code"`
		Replace bool   `json:"replace"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	link := strings.TrimSpace(body.URL)
	if link == "" {
		writeErr(w, http.StatusBadRequest, errors.New("paste the pairing link from the other device"))
		return
	}
	// The link normally carries the code; an explicitly supplied one wins, so
	// a code typed by hand still works.
	code := strings.TrimSpace(body.Code)
	if code == "" {
		code = portable.CodeFromLink(link)
	}
	if code == "" {
		writeErr(w, http.StatusBadRequest, errors.New(
			"that link carries no pairing code — copy the whole link, including everything after the '?'"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	blob, err := portable.Join(ctx, link, code)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	// The code that fetched the bundle is also the key that opens it.
	s.applyBundle(w, blob, portable.NormalizeCode(code), body.Replace)
}

// ensurePairListener starts the LAN-facing pairing listener on demand and
// returns its port. It stays up only while offers are outstanding.
func (s *Server) ensurePairListener() (int, error) {
	s.pairMu.Lock()
	defer s.pairMu.Unlock()
	if s.pairListener != nil {
		return s.pairPort, nil
	}

	ln, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		return 0, fmt.Errorf("could not open a pairing port: %w", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	s.pairListener, s.pairPort = ln, port

	mux := http.NewServeMux()
	mux.HandleFunc("GET /pair/{token}", func(w http.ResponseWriter, r *http.Request) {
		blob, err := s.pairer.Fetch(r.PathValue("token"), r.URL.Query().Get("code"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", strconv.Itoa(len(blob)))
		_, _ = w.Write(blob)
	})

	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("pairing listener: %v", err)
		}
	}()

	// Close the listener once every offer has been used or has expired, so an
	// idle device is not left with an open port.
	go func() {
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for range t.C {
			if len(s.pairer.Active()) > 0 {
				continue
			}
			s.pairMu.Lock()
			if s.pairListener == ln {
				s.pairListener, s.pairPort = nil, 0
			}
			s.pairMu.Unlock()

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_ = srv.Shutdown(ctx)
			cancel()
			return
		}
	}()
	return port, nil
}

// --- cloud config sync ---

func (s *Server) handleCloudStatus(w http.ResponseWriter, r *http.Request) {
	accountID := r.URL.Query().Get("account")
	if accountID == "" {
		accountID = s.st.Settings().CloudSyncAcct
	}
	if accountID == "" {
		writeJSON(w, http.StatusOK, map[string]any{"configured": false})
		return
	}
	acc, drv, ok := s.account(w, accountID)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	exists, modified, size := portable.Status(ctx, drv)
	writeJSON(w, http.StatusOK, map[string]any{
		"configured": true,
		"account":    acc.Public(),
		"exists":     exists,
		"modified":   modified,
		"size":       size,
		"folder":     portable.ConfigFolder,
		"file":       portable.BundleName,
	})
}

func (s *Server) handleCloudPush(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Account    string `json:"account"`
		Passphrase string `json:"passphrase"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	acc, drv, ok := s.account(w, body.Account)
	if !ok {
		return
	}
	blob, err := portable.Export(s.st, body.Passphrase)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()

	if err := portable.Push(ctx, drv, blob); err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	// Remember the chosen drive so the next push is one tap.
	_ = s.st.Mutate(func(st *store.State) error {
		st.Settings.CloudSyncAcct = acc.ID
		return nil
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "account": acc.Label, "size": len(blob),
		"folder": portable.ConfigFolder, "file": portable.BundleName,
	})
}

func (s *Server) handleCloudPull(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Account    string `json:"account"`
		Passphrase string `json:"passphrase"`
		Replace    bool   `json:"replace"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	_, drv, ok := s.account(w, body.Account)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()

	blob, err := portable.Pull(ctx, drv)
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	s.applyBundle(w, blob, body.Passphrase, body.Replace)
}

// PushConfigTo is the CLI-facing form of cloud push.
func (s *Server) PushConfigTo(ctx context.Context, accountID, passphrase string) error {
	acc, ok := s.st.Account(accountID)
	if !ok {
		return fmt.Errorf("account %s not found", accountID)
	}
	drv, err := s.driver(acc)
	if err != nil {
		return err
	}
	blob, err := portable.Export(s.st, passphrase)
	if err != nil {
		return err
	}
	return portable.Push(ctx, drv, blob)
}

// PullConfigFrom is the CLI-facing form of cloud pull.
func (s *Server) PullConfigFrom(ctx context.Context, accountID, passphrase string, replace bool) (portable.Summary, int, int, error) {
	acc, ok := s.st.Account(accountID)
	if !ok {
		return portable.Summary{}, 0, 0, fmt.Errorf("account %s not found", accountID)
	}
	drv, err := s.driver(acc)
	if err != nil {
		return portable.Summary{}, 0, 0, err
	}
	blob, err := portable.Pull(ctx, drv)
	if err != nil {
		return portable.Summary{}, 0, 0, err
	}
	summary, added, updated, err := portable.Import(s.st, blob, passphrase, replace)
	if err == nil {
		s.invalidateDrivers()
	}
	return summary, added, updated, err
}
