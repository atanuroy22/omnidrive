// Package server exposes the HTTP API and serves the embedded web UI.
package server

import (
	"context"
	"crypto/subtle"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"omnidrive/internal/androidnet"
	"omnidrive/internal/portable"
	"omnidrive/internal/provider"
	"omnidrive/internal/store"
	"omnidrive/internal/web"
)

// Options configures a Server.
type Options struct {
	Addr    string // host:port to listen on
	Token   string // required for non-loopback binds; empty disables auth
	Version string
	Store   *store.Store
	HTTP    *http.Client
}

// Server ties the store, the drivers and the UI together.
type Server struct {
	opts   Options
	st     *store.Store
	hc     *http.Client
	mux    *http.ServeMux
	pairer *portable.Pairer
	jobs   *jobRegistry

	drivers sync.Map // accountID -> provider.Driver

	pairMu       sync.Mutex
	pairListener net.Listener
	pairPort     int
}

// New builds the server and registers every route.
func New(opts Options) *Server {
	if opts.HTTP == nil {
		opts.HTTP = DefaultHTTPClient()
	}
	s := &Server{
		opts:   opts,
		st:     opts.Store,
		hc:     opts.HTTP,
		mux:    http.NewServeMux(),
		pairer: portable.NewPairer(),
		jobs:   newJobRegistry(),
	}
	s.routes()
	return s
}

// DefaultHTTPClient returns a client tuned for mobile networks: generous
// timeouts for large transfers, but aggressive connection health checks so a
// dropped Wi-Fi link fails fast instead of hanging the UI.
func DefaultHTTPClient() *http.Client {
	return &http.Client{
		Timeout: 0, // uploads and downloads are unbounded; ctx governs them
		Transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			// Android keeps its root certificates where Go's GOOS=linux
			// loader does not look, so supply them explicitly. Nil here means
			// "use the platform default", which is correct off Android.
			TLSClientConfig: &tls.Config{
				RootCAs:    androidnet.RootCAs(),
				MinVersion: tls.VersionTLS12,
			},
			DialContext: (&net.Dialer{
				Timeout:   15 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          32,
			MaxIdleConnsPerHost:   8,
			IdleConnTimeout:       60 * time.Second,
			TLSHandshakeTimeout:   15 * time.Second,
			ExpectContinueTimeout: 3 * time.Second,
			ResponseHeaderTimeout: 60 * time.Second,
		},
	}
}

// Handler returns the root handler, wrapped in auth and recovery middleware.
func (s *Server) Handler() http.Handler {
	return s.recover(s.auth(s.mux))
}

func (s *Server) routes() {
	m := s.mux

	// Static UI. Everything unmatched falls through to index.html so the
	// single-page app can own its own routes.
	sub, err := fs.Sub(web.Files, "dist")
	if err != nil {
		panic(fmt.Sprintf("embedded UI is missing: %v", err))
	}
	m.Handle("/", web.SPAHandler(sub))

	m.HandleFunc("GET /api/health", s.handleHealth)
	m.HandleFunc("GET /api/providers", s.handleProviders)

	m.HandleFunc("GET /api/accounts", s.handleAccountsList)
	m.HandleFunc("PATCH /api/accounts/{id}", s.handleAccountPatch)
	m.HandleFunc("DELETE /api/accounts/{id}", s.handleAccountDelete)
	m.HandleFunc("POST /api/accounts/refresh", s.handleAccountsRefresh)
	m.HandleFunc("GET /api/accounts/{id}/trash", s.handleTrashList)
	m.HandleFunc("POST /api/accounts/{id}/trash/empty", s.handleEmptyTrash)
	m.HandleFunc("POST /api/accounts/{id}/trash/restore", s.handleTrashRestore)
	m.HandleFunc("POST /api/accounts/{id}/trash/purge", s.handleTrashPurge)

	m.HandleFunc("POST /api/connect/oauth/start", s.handleOAuthStart)
	m.HandleFunc("GET /oauth/callback", s.handleOAuthCallback)
	m.HandleFunc("POST /api/connect/direct", s.handleConnectDirect)

	m.HandleFunc("POST /api/files/share", s.handleShareLink)
	m.HandleFunc("GET /api/shares", s.handleSharesList)

	m.HandleFunc("GET /api/files", s.handleFilesList)
	m.HandleFunc("GET /api/files/download", s.handleDownload)
	m.HandleFunc("GET /api/files/stream", s.handleStream)
	m.HandleFunc("POST /api/files/mkdir", s.handleMkdir)
	m.HandleFunc("POST /api/files/rename", s.handleRename)
	m.HandleFunc("POST /api/files/delete", s.handleDelete)
	m.HandleFunc("POST /api/files/star", s.handleStar)
	m.HandleFunc("GET /api/starred", s.handleStarred)
	m.HandleFunc("POST /api/upload", s.handleUpload)
	m.HandleFunc("GET /api/events", s.handleEvents)

	m.HandleFunc("GET /api/search", s.handleSearch)
	m.HandleFunc("GET /api/storage", s.handleStorageList)
	m.HandleFunc("POST /api/storage/add", s.handleStorageAdd)
	m.HandleFunc("POST /api/transfer", s.handleTransfer)
	m.HandleFunc("GET /api/transfer/targets", s.handleTransferTargets)

	m.HandleFunc("GET /api/settings", s.handleSettingsGet)
	m.HandleFunc("PUT /api/settings", s.handleSettingsPut)
	m.HandleFunc("DELETE /api/settings/oauth/{kind}", s.handleOAuthAppClear)

	m.HandleFunc("POST /api/portable/export", s.handleExport)
	m.HandleFunc("GET /api/portable/export", s.handleExport)
	m.HandleFunc("POST /api/portable/import", s.handleImport)
	m.HandleFunc("POST /api/portable/pair/start", s.handlePairStart)
	m.HandleFunc("POST /api/portable/pair/join", s.handlePairJoin)
	m.HandleFunc("GET /api/portable/cloud/status", s.handleCloudStatus)
	m.HandleFunc("POST /api/portable/cloud/push", s.handleCloudPush)
	m.HandleFunc("POST /api/portable/cloud/pull", s.handleCloudPull)
}

// auth gates the API when the server is reachable from outside this device.
// A loopback-only bind needs no token: anything that can reach it already runs
// as the user.
func (s *Server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.opts.Token == "" {
			next.ServeHTTP(w, r)
			return
		}
		supplied := r.Header.Get("X-OmniDrive-Token")
		if supplied == "" {
			if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
				supplied = strings.TrimPrefix(h, "Bearer ")
			}
		}
		if supplied == "" {
			supplied = r.URL.Query().Get("t")
		}
		if subtle.ConstantTimeCompare([]byte(supplied), []byte(s.opts.Token)) != 1 {
			http.Error(w, "unauthorized: append ?t=<token> to the URL", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// recover turns a panic in any handler into a 500 instead of killing the
// process — on a phone, a crashed server means the user loses their session.
func (s *Server) recover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				log.Printf("panic serving %s %s: %v", r.Method, r.URL.Path, v)
				http.Error(w, "internal error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// driver returns a cached driver for an account. Caching matters for
// correctness as well as speed: a driver owns the mutex that serialises token
// refreshes, so two requests must share one instance.
func (s *Server) driver(acc *store.Account) (provider.Driver, error) {
	if d, ok := s.drivers.Load(acc.ID); ok {
		return d.(provider.Driver), nil
	}
	d, err := provider.Open(acc.Kind, provider.Config{
		AccountID: acc.ID,
		Creds:     acc.Creds,
		HTTP:      s.hc,
		Save: func(c provider.Credentials) error {
			return s.st.SaveCreds(acc.ID, c)
		},
	})
	if err != nil {
		return nil, err
	}
	actual, _ := s.drivers.LoadOrStore(acc.ID, d)
	return actual.(provider.Driver), nil
}

// invalidateDrivers drops cached drivers, needed after an import replaces
// credentials underneath us.
func (s *Server) invalidateDrivers() {
	s.drivers.Range(func(k, _ any) bool {
		s.drivers.Delete(k)
		return true
	})
}

// account resolves an account ID from a request, or writes an error.
func (s *Server) account(w http.ResponseWriter, id string) (*store.Account, provider.Driver, bool) {
	if id == "" {
		writeErr(w, http.StatusBadRequest, errors.New("account is required"))
		return nil, nil, false
	}
	acc, ok := s.st.Account(id)
	if !ok {
		writeErr(w, http.StatusNotFound, fmt.Errorf("account %s not found", id))
		return nil, nil, false
	}
	drv, err := s.driver(acc)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return nil, nil, false
	}
	return acc, drv, true
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	certCount, certSource := androidnet.CertsLoaded()
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":          true,
		"version":     s.opts.Version,
		"accounts":    len(s.st.Accounts()),
		"dnsPatched":  androidnet.Patched(),
		"dnsServers":  androidnet.Servers(),
		"rootCerts":   certCount,
		"certSource":  certSource,
		"dataDir":     s.st.Dir(),
		"tokenNeeded": s.opts.Token != "",
	})
}

func (s *Server) handleProviders(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, provider.Descriptors())
}

func (s *Server) handleAccountsList(w http.ResponseWriter, r *http.Request) {
	accs := s.st.Accounts()
	out := make([]map[string]any, 0, len(accs))
	for _, a := range accs {
		m := a.Public()
		m["displayName"] = a.Creds[provider.CredAccountName]
		// Report what this backend can actually do, so the UI offers only
		// actions that will succeed rather than failing after the tap.
		if drv, err := s.driver(a); err == nil {
			m["capabilities"] = provider.CapabilitiesOf(drv)
		}
		out = append(out, m)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleAccountPatch(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var body struct {
		Label   *string `json:"label"`
		Enabled *bool   `json:"enabled"`
		Weight  *int    `json:"weight"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	err := s.st.Mutate(func(st *store.State) error {
		for _, a := range st.Accounts {
			if a.ID != id {
				continue
			}
			if body.Label != nil && strings.TrimSpace(*body.Label) != "" {
				a.Label = strings.TrimSpace(*body.Label)
			}
			if body.Enabled != nil {
				a.Enabled = *body.Enabled
			}
			if body.Weight != nil && *body.Weight > 0 {
				a.Weight = *body.Weight
			}
			return nil
		}
		return fmt.Errorf("account %s not found", id)
	})
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleAccountDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.st.RemoveAccount(id); err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	s.drivers.Delete(id)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleAccountsRefresh re-measures every account's quota in parallel.
func (s *Server) handleAccountsRefresh(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	s.RefreshQuotas(ctx)
	s.handleAccountsList(w, r)
}

// handleEmptyTrash discards a drive's recycle bin.
//
// Providers that trash rather than delete keep those files counting against
// the quota indefinitely, so without this a user can delete everything in the
// app and still be full.
func (s *Server) handleEmptyTrash(w http.ResponseWriter, r *http.Request) {
	acc, drv, ok := s.account(w, r.PathValue("id"))
	if !ok {
		return
	}
	emptier, supported := drv.(provider.TrashEmptier)
	if !supported {
		writeErr(w, http.StatusBadRequest, fmt.Errorf(
			"%s does not offer a way to empty its recycle bin from outside its own app", acc.Label))
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()

	if err := emptier.EmptyTrash(ctx); err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	// The freed space only shows once the quota is re-measured.
	if q, qerr := drv.Quota(ctx); qerr == nil {
		_ = s.st.SetQuota(acc.ID, q)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "drive": acc.Label})
}

// RefreshQuotas updates cached quota for all enabled accounts. Exported so the
// background scheduler can call it too.
func (s *Server) RefreshQuotas(ctx context.Context) {
	var wg sync.WaitGroup
	for _, acc := range s.st.EnabledAccounts() {
		acc := acc
		drv, err := s.driver(acc)
		if err != nil {
			log.Printf("quota: open %s: %v", acc.Label, err)
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			q, err := drv.Quota(ctx)
			if err != nil {
				log.Printf("quota: %s: %v", acc.Label, err)
				return
			}
			if err := s.st.SetQuota(acc.ID, q); err != nil {
				log.Printf("quota: persist %s: %v", acc.Label, err)
			}
		}()
	}
	wg.Wait()
}

func (s *Server) handleSettingsGet(w http.ResponseWriter, r *http.Request) {
	set := s.st.Settings()
	// Client secrets never leave the device; report only whether each provider
	// is ready to use — either registered here or compiled into this build.
	apps := map[string]bool{}
	for _, d := range provider.Descriptors() {
		if _, _, ok := provider.BuiltinApp(d.Kind); ok {
			apps[string(d.Kind)] = true
		}
	}
	// Separately report which are stored here, since only those can be edited
	// or cleared — a compiled-in credential is fixed until the next build.
	stored := map[string]string{}
	for k, v := range set.OAuthApps {
		if v.ClientID != "" {
			apps[string(k)] = true
			// Enough of the ID to recognise a wrong paste, not the whole thing.
			stored[string(k)] = maskClientID(v.ClientID)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"strategy":            set.Strategy,
		"manualOrder":         set.ManualOrder,
		"theme":               set.Theme,
		"syncIntervalMinutes": set.SyncIntervalM,
		"cloudSyncAccount":    set.CloudSyncAcct,
		"oauthConfigured":     apps,
		"oauthStored":         stored,
		"poolEnabled":         !set.PoolDisabled,
	})
}

// maskClientID shows enough to recognise a wrong paste without printing the
// whole identifier back into the UI.
func maskClientID(id string) string {
	if len(id) <= 10 {
		return id
	}
	return id[:6] + "…" + id[len(id)-4:]
}

// handleOAuthAppClear forgets a provider's stored client credentials so a
// mistyped one can be replaced. Connected accounts keep working: they carry
// their own copy of the credentials alongside their tokens.
func (s *Server) handleOAuthAppClear(w http.ResponseWriter, r *http.Request) {
	kind := provider.Kind(r.PathValue("kind"))
	if _, ok := provider.OAuthEndpoints(kind); !ok {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("%q is not an OAuth provider", kind))
		return
	}
	if err := s.st.ClearOAuthApp(kind); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	_, _, builtin := provider.BuiltinApp(kind)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "kind": kind, "fallsBackToBuiltin": builtin,
	})
}

func (s *Server) handleSettingsPut(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Strategy         *string   `json:"strategy"`
		ManualOrder      *[]string `json:"manualOrder"`
		Theme            *string   `json:"theme"`
		SyncInterval     *int      `json:"syncIntervalMinutes"`
		CloudSyncAccount *string   `json:"cloudSyncAccount"`
		PoolEnabled      *bool     `json:"poolEnabled"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	err := s.st.Mutate(func(st *store.State) error {
		if body.Strategy != nil {
			switch *body.Strategy {
			case store.StrategyRoundRobin, store.StrategyWeighted, store.StrategyLeastUsed,
				store.StrategyMostFree, store.StrategyManual:
				st.Settings.Strategy = *body.Strategy
			default:
				return fmt.Errorf("unknown strategy %q", *body.Strategy)
			}
		}
		if body.ManualOrder != nil {
			st.Settings.ManualOrder = *body.ManualOrder
		}
		if body.Theme != nil {
			st.Settings.Theme = *body.Theme
		}
		if body.SyncInterval != nil && *body.SyncInterval >= 0 {
			st.Settings.SyncIntervalM = *body.SyncInterval
		}
		if body.CloudSyncAccount != nil {
			st.Settings.CloudSyncAcct = *body.CloudSyncAccount
		}
		if body.PoolEnabled != nil {
			st.Settings.PoolDisabled = !*body.PoolEnabled
		}
		return nil
	})
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	s.handleSettingsGet(w, r)
}

// --- small helpers shared by every handler ---

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("write json: %v", err)
	}
}

func writeErr(w http.ResponseWriter, status int, err error) {
	if errors.Is(err, provider.ErrNotFound) {
		status = http.StatusNotFound
	}
	writeJSON(w, status, map[string]any{"error": err.Error()})
}

func decodeBody(w http.ResponseWriter, r *http.Request, out any) bool {
	defer r.Body.Close()
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20))
	if err := dec.Decode(out); err != nil {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("invalid JSON body: %w", err))
		return false
	}
	return true
}
