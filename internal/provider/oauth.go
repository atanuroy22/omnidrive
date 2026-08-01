package provider

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Credential keys shared by every OAuth backend.
const (
	CredClientID     = "client_id"
	CredClientSecret = "client_secret"
	CredAccessToken  = "access_token"
	CredRefreshToken = "refresh_token"
	CredExpiry       = "expiry"
	CredAccountName  = "account_name"
)

// Endpoints describes a provider's OAuth 2.0 configuration.
type Endpoints struct {
	AuthURL   string
	TokenURL  string
	Scopes    []string
	AuthExtra map[string]string // provider-specific authorize params
	PKCE      bool
}

// ScopeMode selects how much access to ask a provider for.
//
// This is not a preference so much as an escape hatch from Google's review
// process. The full Drive scope is "restricted": an app using it cannot leave
// Testing status without a security assessment, and Testing status caps every
// refresh token at 7 days — so the account silently dies weekly. The narrow
// scope is non-sensitive, needs no verification, and its tokens do not expire.
type ScopeMode string

const (
	// ScopeFull sees everything already in the drive.
	ScopeFull ScopeMode = "full"
	// ScopeAppFiles sees only what OmniDrive itself put there.
	ScopeAppFiles ScopeMode = "appfiles"
)

// ScopeChoice describes one option for the UI.
type ScopeChoice struct {
	Mode    ScopeMode `json:"mode"`
	Label   string    `json:"label"`
	Detail  string    `json:"detail"`
	Default bool      `json:"default,omitempty"`
}

// ScopeChoices returns the access levels a provider offers, or nil when there
// is only one sensible option.
func ScopeChoices(kind Kind) []ScopeChoice {
	if kind != KindGoogleDrive {
		return nil
	}
	return []ScopeChoice{
		{
			Mode:  ScopeAppFiles,
			Label: "Only files OmniDrive creates",
			Detail: "No Google verification needed, and the connection never expires. " +
				"You can upload, browse and manage anything OmniDrive puts there, " +
				"but not files already in your Drive.",
			Default: true,
		},
		{
			Mode:  ScopeFull,
			Label: "Full Drive access",
			Detail: "Browse everything already in your Drive. Google classes this as a " +
				"restricted scope: you must add yourself under Test users, and Google " +
				"expires the connection every 7 days until the app passes verification.",
		},
	}
}

// ScopedEndpoints returns a provider's OAuth configuration for a chosen access
// level.
func ScopedEndpoints(kind Kind, mode ScopeMode) (Endpoints, bool) {
	ep, ok := OAuthEndpoints(kind)
	if !ok {
		return ep, false
	}
	if kind == KindGoogleDrive && mode == ScopeAppFiles {
		// drive.file and userinfo.email are both non-sensitive, so a project
		// using only these can be published without review.
		ep.Scopes = []string{
			"https://www.googleapis.com/auth/drive.file",
			"https://www.googleapis.com/auth/userinfo.email",
		}
	}
	return ep, true
}

// OAuthEndpoints returns the configuration for an OAuth-based provider.
func OAuthEndpoints(kind Kind) (Endpoints, bool) {
	switch kind {
	case KindGoogleDrive:
		return Endpoints{
			AuthURL:  "https://accounts.google.com/o/oauth2/v2/auth",
			TokenURL: "https://oauth2.googleapis.com/token",
			Scopes: []string{
				"https://www.googleapis.com/auth/drive",
				"https://www.googleapis.com/auth/userinfo.email",
			},
			// access_type=offline + prompt=consent is the only combination that
			// reliably returns a refresh token on re-authorisation.
			AuthExtra: map[string]string{"access_type": "offline", "prompt": "consent"},
			PKCE:      true,
		}, true
	case KindOneDrive:
		return Endpoints{
			AuthURL:  "https://login.microsoftonline.com/common/oauth2/v2.0/authorize",
			TokenURL: "https://login.microsoftonline.com/common/oauth2/v2.0/token",
			// offline_access is what yields the refresh token on Microsoft.
			Scopes:    []string{"offline_access", "User.Read", "Files.ReadWrite.All"},
			AuthExtra: map[string]string{"response_mode": "query"},
			PKCE:      true,
		}, true
	case KindDropbox:
		return Endpoints{
			AuthURL:  "https://www.dropbox.com/oauth2/authorize",
			TokenURL: "https://api.dropboxapi.com/oauth2/token",
			Scopes: []string{
				"account_info.read", "files.metadata.read", "files.metadata.write",
				"files.content.read", "files.content.write",
			},
			// Without token_access_type=offline Dropbox issues a 4-hour token
			// and no refresh token, and the account silently dies overnight.
			AuthExtra: map[string]string{"token_access_type": "offline"},
			PKCE:      true,
		}, true
	}
	return Endpoints{}, false
}

// PKCEPair is a generated code_verifier / code_challenge pair.
type PKCEPair struct {
	Verifier  string
	Challenge string
}

// NewPKCE generates a fresh verifier and its S256 challenge.
func NewPKCE() (PKCEPair, error) {
	b := make([]byte, 48)
	if _, err := rand.Read(b); err != nil {
		return PKCEPair{}, err
	}
	verifier := base64.RawURLEncoding.EncodeToString(b)
	sum := sha256.Sum256([]byte(verifier))
	return PKCEPair{Verifier: verifier, Challenge: base64.RawURLEncoding.EncodeToString(sum[:])}, nil
}

// AuthCodeURL builds the URL the user opens to grant access.
func AuthCodeURL(ep Endpoints, clientID, redirectURI, state, challenge string) string {
	q := url.Values{}
	q.Set("client_id", clientID)
	q.Set("redirect_uri", redirectURI)
	q.Set("response_type", "code")
	q.Set("state", state)
	if len(ep.Scopes) > 0 {
		q.Set("scope", strings.Join(ep.Scopes, " "))
	}
	for k, v := range ep.AuthExtra {
		q.Set(k, v)
	}
	if ep.PKCE && challenge != "" {
		q.Set("code_challenge", challenge)
		q.Set("code_challenge_method", "S256")
	}
	return ep.AuthURL + "?" + q.Encode()
}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
	TokenType    string `json:"token_type"`
	Scope        string `json:"scope"`
	Error        string `json:"error"`
	ErrorDesc    string `json:"error_description"`
}

// ExchangeCode trades an authorization code for tokens and returns the
// credentials to store on the account.
func ExchangeCode(ctx context.Context, hc *http.Client, ep Endpoints, clientID, clientSecret, code, redirectURI, verifier string) (Credentials, error) {
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", redirectURI)
	form.Set("client_id", clientID)
	if clientSecret != "" {
		form.Set("client_secret", clientSecret)
	}
	if verifier != "" {
		form.Set("code_verifier", verifier)
	}

	tok, err := postToken(ctx, hc, ep.TokenURL, form)
	if err != nil {
		return nil, err
	}
	if tok.RefreshToken == "" {
		return nil, errors.New("provider returned no refresh token; the account would stop working within hours. " +
			"Revoke the app's access in your provider account settings and connect again")
	}
	creds := Credentials{
		CredClientID:     clientID,
		CredClientSecret: clientSecret,
		CredAccessToken:  tok.AccessToken,
		CredRefreshToken: tok.RefreshToken,
		CredExpiry:       expiryFrom(tok.ExpiresIn).Format(time.RFC3339),
	}
	return creds, nil
}

func expiryFrom(expiresIn int64) time.Time {
	if expiresIn <= 0 {
		expiresIn = 3600
	}
	return time.Now().Add(time.Duration(expiresIn) * time.Second).UTC()
}

func postToken(ctx context.Context, hc *http.Client, tokenURL string, form url.Values) (*tokenResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var tok tokenResponse
	dec := json.NewDecoder(resp.Body)
	if err := dec.Decode(&tok); err != nil && resp.StatusCode < 300 {
		return nil, fmt.Errorf("token endpoint returned unparseable body: %w", err)
	}
	if tok.Error != "" {
		return nil, fmt.Errorf("oauth %s: %s", tok.Error, tok.ErrorDesc)
	}
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("token endpoint: HTTP %d", resp.StatusCode)
	}
	if tok.AccessToken == "" {
		return nil, errors.New("token endpoint returned no access token")
	}
	return &tok, nil
}

// authClient wraps an *http.Client with automatic bearer-token injection and
// refresh. Every OAuth driver embeds one.
type authClient struct {
	cfg Config
	ep  Endpoints

	mu sync.Mutex
}

func newAuthClient(cfg Config, ep Endpoints) *authClient {
	return &authClient{cfg: cfg, ep: ep}
}

// token returns a valid access token, refreshing if it expires within a minute.
func (a *authClient) token(ctx context.Context) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	access := a.cfg.Creds[CredAccessToken]
	exp, _ := time.Parse(time.RFC3339, a.cfg.Creds[CredExpiry])
	if access != "" && time.Until(exp) > time.Minute {
		return access, nil
	}
	return a.refreshLocked(ctx)
}

func (a *authClient) refreshLocked(ctx context.Context) (string, error) {
	refresh := a.cfg.Creds[CredRefreshToken]
	if refresh == "" {
		return "", errors.New("account has no refresh token; reconnect it")
	}
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", refresh)
	form.Set("client_id", a.cfg.Creds[CredClientID])
	if s := a.cfg.Creds[CredClientSecret]; s != "" {
		form.Set("client_secret", s)
	}
	// No scope parameter: on a refresh it defaults to whatever was originally
	// granted, and naming a broader set than the user actually approved is an
	// error. That would break every account connected with a narrow scope.

	tok, err := postToken(ctx, a.cfg.HTTP, a.ep.TokenURL, form)
	if err != nil {
		return "", fmt.Errorf("refresh access token: %w", err)
	}
	a.cfg.Creds[CredAccessToken] = tok.AccessToken
	a.cfg.Creds[CredExpiry] = expiryFrom(tok.ExpiresIn).Format(time.RFC3339)
	// Google and Microsoft rotate refresh tokens on some flows; keep the new
	// one or the account dies at the next refresh.
	if tok.RefreshToken != "" {
		a.cfg.Creds[CredRefreshToken] = tok.RefreshToken
	}
	if err := a.cfg.Save(a.cfg.Creds); err != nil {
		return "", fmt.Errorf("persist refreshed token: %w", err)
	}
	return tok.AccessToken, nil
}

// forceRefresh discards the cached token, used after an unexpected 401.
func (a *authClient) forceRefresh(ctx context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	_, err := a.refreshLocked(ctx)
	return err
}

// do sends req with a bearer token attached, retrying once after a refresh if
// the provider rejects the token, and backing off on transient failures.
//
// build must produce a fresh request each call because a retried request needs
// an unconsumed body.
func (a *authClient) do(ctx context.Context, build func() (*http.Request, error)) (*http.Response, error) {
	const maxAttempts = 4
	var lastErr error

	for attempt := 0; attempt < maxAttempts; attempt++ {
		tok, err := a.token(ctx)
		if err != nil {
			return nil, err
		}
		req, err := build()
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+tok)

		resp, err := a.cfg.HTTP.Do(req)
		if err == nil && resp.StatusCode == http.StatusUnauthorized {
			resp.Body.Close()
			if rerr := a.forceRefresh(ctx); rerr != nil {
				return nil, rerr
			}
			lastErr = errors.New("unauthorized")
			continue
		}
		if !retryable(resp, err) {
			return resp, err
		}
		if resp != nil {
			// Preserve the final response so the caller sees the real error.
			if attempt == maxAttempts-1 {
				return resp, nil
			}
			resp.Body.Close()
		}
		lastErr = err
		if berr := backoff(ctx, attempt); berr != nil {
			return nil, berr
		}
	}
	if lastErr == nil {
		lastErr = errors.New("request failed after retries")
	}
	return nil, lastErr
}

// doJSON is do() plus JSON decoding and status checking.
func (a *authClient) doJSON(ctx context.Context, build func() (*http.Request, error), out any) error {
	resp, err := a.do(ctx, build)
	if err != nil {
		return err
	}
	if err := checkResponse(resp); err != nil {
		return err
	}
	defer resp.Body.Close()
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
