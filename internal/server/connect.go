package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"omnidrive/internal/provider"
	"omnidrive/internal/store"
)

// pending holds one in-flight OAuth authorisation. Entries are short-lived and
// keyed by the state parameter, which doubles as CSRF protection: a callback
// carrying a state we did not issue is rejected.
type pending struct {
	kind        provider.Kind
	verifier    string
	clientID    string
	secret      string
	redirectURI string
	createdAt   time.Time
}

var (
	pendingMu sync.Mutex
	pendings  = map[string]*pending{}
)

const pendingTTL = 15 * time.Minute

func putPending(state string, p *pending) {
	pendingMu.Lock()
	defer pendingMu.Unlock()
	// Opportunistic sweep; there are never more than a handful of these.
	for k, v := range pendings {
		if time.Since(v.createdAt) > pendingTTL {
			delete(pendings, k)
		}
	}
	pendings[state] = p
}

func takePending(state string) (*pending, bool) {
	pendingMu.Lock()
	defer pendingMu.Unlock()
	p, ok := pendings[state]
	if ok {
		delete(pendings, state)
	}
	if ok && time.Since(p.createdAt) > pendingTTL {
		return nil, false
	}
	return p, ok
}

// sanitizeCredential cleans a value pasted from a downloaded JSON file. People
// copy straight out of client_secret_*.json and bring the surrounding quotes
// and trailing comma with them; a stray quote in client_id makes Google reject
// the whole authorize request as malformed, with no hint as to why.
func sanitizeCredential(raw, label string) (string, error) {
	v := strings.TrimSpace(raw)
	v = strings.Trim(v, ",")
	v = strings.TrimSpace(v)
	// Strip a matched pair of surrounding quotes, then any stragglers.
	for _, q := range []string{`"`, `'`} {
		if len(v) >= 2 && strings.HasPrefix(v, q) && strings.HasSuffix(v, q) {
			v = v[1 : len(v)-1]
		}
	}
	v = strings.Trim(v, `"'`)
	v = strings.TrimSpace(v)

	if v == "" {
		return "", nil
	}
	// Anything left that cannot appear in a URL query value is a paste error.
	for _, r := range v {
		if r < 0x20 || r == 0x7F || r == ' ' || r == '"' || r == '\'' || r == '\\' {
			return "", fmt.Errorf("that %s contains an invalid character — "+
				"paste only the value itself, without quotes or surrounding JSON", label)
		}
	}
	return v, nil
}

// validateClientID catches the common wrong-field pastes before the user is
// sent to a provider that will only answer with a generic error page.
func validateClientID(kind provider.Kind, clientID string) error {
	if clientID == "" {
		return nil // handled by the caller, which may fall back to a stored one
	}
	switch kind {
	case provider.KindGoogleDrive:
		if strings.HasPrefix(clientID, "GOCSPX-") {
			return errors.New("that is the client secret, not the client ID. " +
				"The client ID ends in .apps.googleusercontent.com")
		}
		if !strings.HasSuffix(clientID, ".apps.googleusercontent.com") {
			return errors.New("a Google client ID must end in .apps.googleusercontent.com — " +
				"copy the whole value from the credentials page")
		}
	case provider.KindOneDrive:
		// Microsoft application IDs are GUIDs.
		if len(clientID) != 36 || strings.Count(clientID, "-") != 4 {
			return errors.New("a Microsoft application (client) ID is a GUID like " +
				"00000000-1111-2222-3333-444444444444")
		}
	}
	return nil
}

// redirectURI is the loopback URL the provider sends the user back to. It must
// match what was registered with the provider byte for byte.
func (s *Server) redirectURI(r *http.Request) string {
	host := r.Host
	if host == "" {
		host = s.opts.Addr
	}
	return "http://" + host + "/oauth/callback"
}

// handleOAuthStart begins an authorisation. The client credentials the user
// supplies are saved into settings so they end up in portable bundles — that
// is what lets a restored device re-authorise without visiting a developer
// console again.
func (s *Server) handleOAuthStart(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Kind         string `json:"kind"`
		ClientID     string `json:"clientId"`
		ClientSecret string `json:"clientSecret"`
		ScopeMode    string `json:"scopeMode"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	kind := provider.Kind(body.Kind)

	mode := provider.ScopeMode(body.ScopeMode)
	if mode == "" {
		mode = provider.ScopeFull
		// Prefer whichever level the provider marks as the safe default.
		for _, c := range provider.ScopeChoices(kind) {
			if c.Default {
				mode = c.Mode
			}
		}
	}
	ep, ok := provider.ScopedEndpoints(kind, mode)
	if !ok {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("%q is not an OAuth provider", body.Kind))
		return
	}

	clientID, err := sanitizeCredential(body.ClientID, "client ID")
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	secret, err := sanitizeCredential(body.ClientSecret, "client secret")
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := validateClientID(kind, clientID); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	// Previously registered credentials, so connecting a second account of the
	// same kind needs no re-entry.
	if clientID == "" {
		if app, ok := s.st.OAuthApp(kind); ok {
			clientID, secret = app.ClientID, app.ClientSecret
		}
	}
	// Credentials compiled into this build: a binary built with an oauth.env
	// never asks the user for anything.
	if clientID == "" {
		if id, sec, ok := provider.BuiltinApp(kind); ok {
			clientID, secret = id, sec
		}
	}
	if clientID == "" {
		writeErr(w, http.StatusBadRequest, errors.New("client ID is required the first time you connect this provider"))
		return
	}
	if err := s.st.SetOAuthApp(kind, store.OAuthApp{ClientID: clientID, ClientSecret: secret}); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}

	pkce, err := provider.NewPKCE()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	state := store.NewID() + store.NewID()
	redirect := s.redirectURI(r)
	putPending(state, &pending{
		kind: kind, verifier: pkce.Verifier, clientID: clientID,
		secret: secret, redirectURI: redirect, createdAt: time.Now(),
	})

	writeJSON(w, http.StatusOK, map[string]any{
		"authUrl":     provider.AuthCodeURL(ep, clientID, redirect, state, pkce.Challenge),
		"redirectUri": redirect,
		"state":       state,
	})
}

// handleOAuthCallback completes the flow and creates the account. It renders
// HTML rather than JSON because the user's browser lands here directly.
func (s *Server) handleOAuthCallback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if e := q.Get("error"); e != "" {
		renderResult(w, false, fmt.Sprintf("%s: %s", e, q.Get("error_description")))
		return
	}
	code, state := q.Get("code"), q.Get("state")
	if code == "" || state == "" {
		renderResult(w, false, "The provider did not return an authorization code.")
		return
	}
	p, ok := takePending(state)
	if !ok {
		renderResult(w, false, "This sign-in link has expired or was not started here. Try connecting again.")
		return
	}
	ep, _ := provider.OAuthEndpoints(p.kind)

	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	creds, err := provider.ExchangeCode(ctx, s.hc, ep, p.clientID, p.secret, code, p.redirectURI, p.verifier)
	if err != nil {
		renderResult(w, false, err.Error())
		return
	}

	// Ask the provider who we just signed in as *before* saving anything. The
	// answer is what identifies the account, so reconnecting the same drive
	// updates it rather than adding a duplicate.
	label := defaultLabel(p.kind)
	probe, perr := provider.Open(p.kind, provider.Config{
		Creds: creds, HTTP: s.hc,
		Save: func(c provider.Credentials) error { creds = c; return nil },
	})
	if perr == nil {
		if name := provider.AccountName(ctx, probe); name != "" {
			label = name
		}
	}

	acc, isNew, err := s.upsertAccount(p.kind, label, creds)
	if err != nil {
		renderResult(w, false, err.Error())
		return
	}
	if drv, derr := s.driver(acc); derr == nil {
		if q, qerr := drv.Quota(ctx); qerr == nil {
			_ = s.st.SetQuota(acc.ID, q)
		}
	}
	if !isNew {
		renderResult(w, true, "reconnected")
		return
	}
	renderResult(w, true, "")
}

func defaultLabel(kind provider.Kind) string {
	for _, d := range provider.Descriptors() {
		if d.Kind == kind {
			return d.Label
		}
	}
	return string(kind)
}

// handleConnectDirect adds a non-OAuth account (pCloud, WebDAV, S3). The
// credentials are validated by making one real call before anything is saved,
// so a typo surfaces immediately instead of as a broken drive later.
func (s *Server) handleConnectDirect(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Kind   string            `json:"kind"`
		Label  string            `json:"label"`
		Fields map[string]string `json:"fields"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	kind := provider.Kind(body.Kind)

	var desc *provider.Descriptor
	for _, d := range provider.Descriptors() {
		if d.Kind == kind {
			dd := d
			desc = &dd
			break
		}
	}
	if desc == nil {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("unknown provider %q", body.Kind))
		return
	}
	if desc.Auth == "oauth" {
		writeErr(w, http.StatusBadRequest, errors.New("this provider uses OAuth; use the sign-in flow"))
		return
	}
	creds := provider.Credentials{}
	for _, f := range desc.Fields {
		v := strings.TrimSpace(body.Fields[f.Key])
		if f.Required && v == "" {
			writeErr(w, http.StatusBadRequest, fmt.Errorf("%s is required", f.Label))
			return
		}
		creds[f.Key] = v
	}

	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()

	drv, err := provider.Open(kind, provider.Config{
		Creds: creds, HTTP: s.hc,
		Save: func(c provider.Credentials) error { creds = c; return nil },
	})
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	// A listing is the cheapest proof that the credentials work and the
	// path/bucket actually exists.
	if _, err := drv.List(ctx, ""); err != nil {
		writeErr(w, http.StatusBadGateway, fmt.Errorf("could not read this storage: %w", err))
		return
	}

	// Learn the signed-in identity where the provider offers one, so the same
	// account connected twice updates instead of duplicating.
	if name := provider.AccountName(ctx, drv); name != "" {
		creds[provider.CredAccountName] = name
	}

	label := strings.TrimSpace(body.Label)
	if label == "" {
		label = suggestLabel(kind, creds)
	}
	acc, isNew, err := s.upsertAccount(kind, label, creds)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if q, qerr := drv.Quota(ctx); qerr == nil {
		_ = s.st.SetQuota(acc.ID, q)
	}
	out := acc.Public()
	out["reconnected"] = !isNew
	writeJSON(w, http.StatusOK, out)
}

func suggestLabel(kind provider.Kind, creds provider.Credentials) string {
	switch kind {
	case provider.KindS3:
		return "S3: " + creds["bucket"]
	case provider.KindWebDAV:
		if u := creds["url"]; u != "" {
			trimmed := strings.TrimPrefix(strings.TrimPrefix(u, "https://"), "http://")
			if i := strings.Index(trimmed, "/"); i > 0 {
				trimmed = trimmed[:i]
			}
			return trimmed
		}
	case provider.KindPCloud:
		if u := creds["username"]; u != "" {
			return u
		}
	case provider.KindTeraBox:
		if n := creds[provider.CredAccountName]; n != "" {
			return n
		}
	}
	return defaultLabel(kind)
}

// renderResult is the page the OAuth provider redirects the user back to.
// The user's browser lands here directly, so it is HTML rather than JSON.
func renderResult(w http.ResponseWriter, ok bool, msg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	title, icon, colour := "Account connected", "✓", "#3fb950"
	body := "You can close this tab and go back to OmniDrive."
	hint := ""

	if ok && msg == "reconnected" {
		title = "Drive reconnected"
		body = "This account was already connected, so its sign-in was refreshed " +
			"rather than adding a second copy. You can close this tab."
	}

	if !ok {
		w.WriteHeader(http.StatusBadRequest)
		title, icon, colour = "Could not connect", "✕", "#f85149"
		body = htmlEscape(msg)
		// Nearly every failure here is a mistyped client ID or a redirect URI
		// that was not registered. Both are recoverable, but only if the user
		// knows where the escape hatch is.
		hint = `<p class="hint">Wrong client ID? In OmniDrive go to <b>Drives → ` +
			`Connect a drive</b>, pick the provider, and choose ` +
			`<b>Clear saved credentials</b> to enter a different one.</p>`
	} else {
		w.WriteHeader(http.StatusOK)
	}

	fmt.Fprintf(w, `<!doctype html><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>%s</title>
<style>
:root{color-scheme:dark}
body{margin:0;min-height:100dvh;display:grid;place-items:center;
 background:#0d1117;color:#e6edf3;font:16px/1.55 system-ui,-apple-system,Segoe UI,Roboto,sans-serif;padding:24px}
.card{max-width:34rem;text-align:center}
.icon{font-size:44px;color:%s}
h1{font-size:1.25rem;margin:.5rem 0}
p{color:#9198a1;word-break:break-word}
.hint{font-size:.85rem;margin-top:1.25rem;padding-top:1rem;border-top:1px solid #2a313c}
a{display:inline-block;margin-top:1.25rem;padding:.6rem 1.2rem;border-radius:8px;
 background:#2f81f7;color:#fff;text-decoration:none;font-weight:600}
</style>
<div class="card"><div class="icon">%s</div><h1>%s</h1><p>%s</p>%s
<a href="/">Back to OmniDrive</a></div>`,
		title, colour, icon, title, body, hint)
}

func htmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;")
	return r.Replace(s)
}
