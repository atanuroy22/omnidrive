// Package store owns all persistent state: connected accounts and their
// secrets, OAuth app registrations, starred files and user settings.
//
// Everything lives in one encrypted blob on disk. Writes are atomic (temp file
// + rename) because a phone can be killed by the OOM killer or lose power
// mid-write, and a truncated state file would mean re-adding every account.
package store

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"omnidrive/internal/provider"
	"omnidrive/internal/vault"
)

const (
	stateFile = "state.omni"
	keyFile   = "device.key"
	aadState  = "omnidrive/state/v1"
)

// Account is one connected cloud drive.
type Account struct {
	ID        string               `json:"id"`
	Kind      provider.Kind        `json:"kind"`
	Label     string               `json:"label"`
	Creds     provider.Credentials `json:"creds"`
	Weight    int                  `json:"weight"`  // for weighted_round_robin
	Enabled   bool                 `json:"enabled"` // excluded from listing/allocation when false
	CreatedAt time.Time            `json:"createdAt"`

	// Cached quota so the UI has something to show before the first refresh.
	QuotaUsed  int64     `json:"quotaUsed"`
	QuotaTotal int64     `json:"quotaTotal"`
	QuotaAt    time.Time `json:"quotaAt"`
}

// Public strips secrets for API responses.
func (a *Account) Public() map[string]any {
	return map[string]any{
		"id": a.ID, "kind": a.Kind, "label": a.Label,
		"weight": a.Weight, "enabled": a.Enabled, "createdAt": a.CreatedAt,
		"quotaUsed": a.QuotaUsed, "quotaTotal": a.QuotaTotal, "quotaAt": a.QuotaAt,
	}
}

// OAuthApp holds the client credentials the user registered with a provider.
// Keeping these in the vault is what makes a restored device work without
// re-registering anything.
type OAuthApp struct {
	ClientID     string `json:"clientId"`
	ClientSecret string `json:"clientSecret"`
}

// Allocation strategies, matching upstream OmniCloud's vocabulary.
const (
	StrategyRoundRobin = "round_robin"
	StrategyWeighted   = "weighted_round_robin"
	StrategyLeastUsed  = "least_used"
	StrategyMostFree   = "most_free"
	StrategyManual     = "manual"
)

// Settings are user preferences.
type Settings struct {
	Strategy      string                     `json:"strategy"`
	ManualOrder   []string                   `json:"manualOrder"`
	OAuthApps     map[provider.Kind]OAuthApp `json:"oauthApps"`
	Theme         string                     `json:"theme"`
	SyncIntervalM int                        `json:"syncIntervalMinutes"`
	CloudSyncAcct string                     `json:"cloudSyncAccount"` // account used for config sync
	// LocalAutoAdded records that device storage was offered once, so removing
	// that drive on purpose is not undone at the next start.
	LocalAutoAdded bool `json:"localAutoAdded"`
	// PoolDisabled turns off the combined "All drives" view. Stored inverted so
	// the zero value — and therefore every existing setup — has it enabled.
	PoolDisabled bool `json:"poolDisabled"`
}

// State is the whole persisted document.
type State struct {
	Version   int          `json:"version"`
	Accounts  []*Account   `json:"accounts"`
	Settings  Settings     `json:"settings"`
	Starred   []string     `json:"starred"` // "accountID/fileID"
	Shares    []SharedLink `json:"shares"`
	RRCursor  int          `json:"rrCursor"`
	UpdatedAt time.Time    `json:"updatedAt"`
}

// SharedLink records a public URL this device handed out.
//
// The provider is the authority on whether a link is live, but none of them
// offers a cheap "list everything that is public" call across all four, and a
// link you cannot find is a link you cannot revoke. So OmniDrive keeps its own
// register of what it published — enough to show a list and to take one back.
type SharedLink struct {
	AccountID string    `json:"accountId"`
	FileID    string    `json:"fileId"`
	Name      string    `json:"name"`
	URL       string    `json:"url"`
	Direct    string    `json:"direct,omitempty"`
	IsDir     bool      `json:"isDir,omitempty"`
	Created   time.Time `json:"created"`
}

// Store is a concurrency-safe handle to the encrypted state document.
type Store struct {
	dir string
	key []byte

	mu    sync.RWMutex
	state State
}

// Open loads (or initialises) the store in dir. A random device key is created
// on first run; pass a non-empty passphrase to protect the key with a password
// instead of leaving it readable to anything running as your user.
func Open(dir, passphrase string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}
	s := &Store{dir: dir}

	key, err := s.loadOrCreateKey(passphrase)
	if err != nil {
		return nil, err
	}
	s.key = key

	blob, err := os.ReadFile(filepath.Join(dir, stateFile))
	switch {
	case errors.Is(err, os.ErrNotExist):
		s.state = State{Version: 1, Settings: defaultSettings()}
		return s, s.save()
	case err != nil:
		return nil, fmt.Errorf("read state: %w", err)
	}

	plain, err := vault.OpenWithKey(s.key, blob, []byte(aadState))
	if err != nil {
		return nil, fmt.Errorf("decrypt state (wrong device key or corrupted file): %w", err)
	}
	if err := json.Unmarshal(plain, &s.state); err != nil {
		return nil, fmt.Errorf("parse state: %w", err)
	}
	s.normalise()
	return s, nil
}

func defaultSettings() Settings {
	return Settings{
		Strategy:      StrategyMostFree,
		OAuthApps:     map[provider.Kind]OAuthApp{},
		Theme:         "dark",
		SyncIntervalM: 30,
	}
}

// normalise repairs anything a hand-edited or older state file might be missing.
func (s *Store) normalise() {
	if s.state.Settings.OAuthApps == nil {
		s.state.Settings.OAuthApps = map[provider.Kind]OAuthApp{}
	}
	if s.state.Settings.Strategy == "" {
		s.state.Settings.Strategy = StrategyMostFree
	}
	if s.state.Settings.SyncIntervalM <= 0 {
		s.state.Settings.SyncIntervalM = 30
	}
	for _, a := range s.state.Accounts {
		if a.Creds == nil {
			a.Creds = provider.Credentials{}
		}
		if a.Weight <= 0 {
			a.Weight = 1
		}
	}
}

func (s *Store) loadOrCreateKey(passphrase string) ([]byte, error) {
	path := filepath.Join(s.dir, keyFile)
	blob, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		key, err := vault.NewKey()
		if err != nil {
			return nil, err
		}
		if err := s.writeKey(path, key, passphrase); err != nil {
			return nil, err
		}
		return key, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read device key: %w", err)
	}

	if vault.IsPassphraseSealed(blob) {
		if passphrase == "" {
			return nil, errors.New("this device's local state is passphrase-protected: pass -device-passphrase or set OMNIDRIVE_DEVICE_PASSPHRASE")
		}
		key, err := vault.Open(passphrase, blob, []byte("omnidrive/devicekey"))
		if err != nil {
			// Say which passphrase failed; this is otherwise easy to confuse
			// with a bundle passphrase.
			return nil, fmt.Errorf("could not unlock this device's local state: %w "+
				"(this is the -device-passphrase, not a bundle passphrase)", err)
		}
		return key, nil
	}
	// Unprotected keys are stored as raw hex so they can be re-created by hand
	// in a recovery situation.
	key, err := hex.DecodeString(strings.TrimSpace(string(blob)))
	if err != nil || len(key) != 32 {
		return nil, errors.New("device key file is malformed")
	}
	return key, nil
}

func (s *Store) writeKey(path string, key []byte, passphrase string) error {
	var out []byte
	if passphrase != "" {
		sealed, err := vault.Seal(passphrase, key, []byte("omnidrive/devicekey"))
		if err != nil {
			return err
		}
		out = sealed
	} else {
		out = []byte(hex.EncodeToString(key) + "\n")
	}
	return atomicWrite(path, out, 0o600)
}

// save serialises and encrypts the current state. Callers hold s.mu.
func (s *Store) save() error {
	s.state.UpdatedAt = time.Now().UTC()
	plain, err := json.Marshal(s.state)
	if err != nil {
		return err
	}
	blob, err := vault.SealWithKey(s.key, plain, []byte(aadState))
	if err != nil {
		return err
	}
	return atomicWrite(filepath.Join(s.dir, stateFile), blob, 0o600)
}

// Mutate applies fn under the write lock and persists the result. If fn
// returns an error nothing is written.
func (s *Store) Mutate(fn func(*State) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := fn(&s.state); err != nil {
		return err
	}
	return s.save()
}

// Read runs fn under the read lock.
func (s *Store) Read(fn func(*State)) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	fn(&s.state)
}

// Dir returns the data directory.
func (s *Store) Dir() string { return s.dir }

// Accounts returns a snapshot of all accounts.
func (s *Store) Accounts() []*Account {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Account, len(s.state.Accounts))
	copy(out, s.state.Accounts)
	return out
}

// EnabledAccounts returns only accounts eligible for listing and allocation.
func (s *Store) EnabledAccounts() []*Account {
	var out []*Account
	for _, a := range s.Accounts() {
		if a.Enabled {
			out = append(out, a)
		}
	}
	return out
}

// Account looks up one account by ID.
func (s *Store) Account(id string) (*Account, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, a := range s.state.Accounts {
		if a.ID == id {
			return a, true
		}
	}
	return nil, false
}

// AddAccount stores a new account and returns it.
func (s *Store) AddAccount(kind provider.Kind, label string, creds provider.Credentials) (*Account, error) {
	acc := &Account{
		ID:        NewID(),
		Kind:      kind,
		Label:     label,
		Creds:     creds,
		Weight:    1,
		Enabled:   true,
		CreatedAt: time.Now().UTC(),
	}
	err := s.Mutate(func(st *State) error {
		// Labels are what the user navigates by, so keep them unique.
		acc.Label = uniqueLabel(st.Accounts, label)
		st.Accounts = append(st.Accounts, acc)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return acc, nil
}

// RemoveAccount deletes an account and any stars pointing at it.
func (s *Store) RemoveAccount(id string) error {
	return s.Mutate(func(st *State) error {
		out := st.Accounts[:0]
		found := false
		for _, a := range st.Accounts {
			if a.ID == id {
				found = true
				continue
			}
			out = append(out, a)
		}
		if !found {
			return fmt.Errorf("account %s not found", id)
		}
		st.Accounts = out

		stars := st.Starred[:0]
		for _, k := range st.Starred {
			if !strings.HasPrefix(k, id+"/") {
				stars = append(stars, k)
			}
		}
		st.Starred = stars

		// The links themselves live on at the provider — disconnecting a drive
		// here cannot revoke them — but listing links to a drive that is gone
		// would offer a revoke button that could never work.
		shares := st.Shares[:0]
		for _, l := range st.Shares {
			if l.AccountID != id {
				shares = append(shares, l)
			}
		}
		st.Shares = shares
		return nil
	})
}

// SaveCreds persists refreshed credentials for an account. Drivers call this
// through provider.Config.Save whenever a token rotates.
func (s *Store) SaveCreds(id string, creds provider.Credentials) error {
	return s.Mutate(func(st *State) error {
		for _, a := range st.Accounts {
			if a.ID == id {
				a.Creds = creds
				return nil
			}
		}
		return fmt.Errorf("account %s not found", id)
	})
}

// SetQuota caches a freshly measured quota.
func (s *Store) SetQuota(id string, q provider.Quota) error {
	return s.Mutate(func(st *State) error {
		for _, a := range st.Accounts {
			if a.ID == id {
				a.QuotaUsed, a.QuotaTotal, a.QuotaAt = q.Used, q.Total, time.Now().UTC()
				return nil
			}
		}
		return nil
	})
}

// Settings returns a copy of current settings.
func (s *Store) Settings() Settings {
	s.mu.RLock()
	defer s.mu.RUnlock()
	cp := s.state.Settings
	cp.OAuthApps = make(map[provider.Kind]OAuthApp, len(s.state.Settings.OAuthApps))
	for k, v := range s.state.Settings.OAuthApps {
		cp.OAuthApps[k] = v
	}
	cp.ManualOrder = append([]string(nil), s.state.Settings.ManualOrder...)
	return cp
}

// OAuthApp returns the registered client credentials for a provider.
func (s *Store) OAuthApp(kind provider.Kind) (OAuthApp, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	app, ok := s.state.Settings.OAuthApps[kind]
	return app, ok && app.ClientID != ""
}

// SetOAuthApp registers client credentials for a provider.
func (s *Store) SetOAuthApp(kind provider.Kind, app OAuthApp) error {
	return s.Mutate(func(st *State) error {
		st.Settings.OAuthApps[kind] = app
		return nil
	})
}

// ClearOAuthApp forgets the stored client credentials for a provider. Without
// this a mistyped client ID would be permanent: it is saved before the sign-in
// that would prove it works, and thereafter reused without prompting.
func (s *Store) ClearOAuthApp(kind provider.Kind) error {
	return s.Mutate(func(st *State) error {
		delete(st.Settings.OAuthApps, kind)
		return nil
	})
}

// StoredOAuthKinds lists providers with credentials saved on this device, as
// opposed to compiled into the build.
func (s *Store) StoredOAuthKinds() []provider.Kind {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []provider.Kind
	for k, v := range s.state.Settings.OAuthApps {
		if v.ClientID != "" {
			out = append(out, k)
		}
	}
	return out
}

// Star toggles the starred flag for a file.
func (s *Store) Star(accountID, fileID string, on bool) error {
	key := accountID + "/" + fileID
	return s.Mutate(func(st *State) error {
		idx := -1
		for i, k := range st.Starred {
			if k == key {
				idx = i
				break
			}
		}
		switch {
		case on && idx < 0:
			st.Starred = append(st.Starred, key)
		case !on && idx >= 0:
			st.Starred = append(st.Starred[:idx], st.Starred[idx+1:]...)
		}
		return nil
	})
}

// IsStarred reports whether a file is starred.
func (s *Store) IsStarred(accountID, fileID string) bool {
	key := accountID + "/" + fileID
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, k := range s.state.Starred {
		if k == key {
			return true
		}
	}
	return false
}

// StarredKeys returns every "accountID/fileID" star.
func (s *Store) StarredKeys() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]string(nil), s.state.Starred...)
}

// AddShare records a published link, replacing any earlier entry for the same
// file so re-sharing does not produce duplicates.
func (s *Store) AddShare(link SharedLink) error {
	if link.Created.IsZero() {
		link.Created = time.Now()
	}
	return s.Mutate(func(st *State) error {
		for i, existing := range st.Shares {
			if existing.AccountID == link.AccountID && existing.FileID == link.FileID {
				st.Shares[i] = link
				return nil
			}
		}
		st.Shares = append(st.Shares, link)
		return nil
	})
}

// RemoveShare forgets a link, after it has been revoked at the provider.
func (s *Store) RemoveShare(accountID, fileID string) error {
	return s.Mutate(func(st *State) error {
		out := st.Shares[:0]
		for _, l := range st.Shares {
			if l.AccountID == accountID && l.FileID == fileID {
				continue
			}
			out = append(out, l)
		}
		st.Shares = out
		return nil
	})
}

// Shares lists every published link, newest first.
func (s *Store) Shares() []SharedLink {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := append([]SharedLink(nil), s.state.Shares...)
	sort.Slice(out, func(i, j int) bool { return out[i].Created.After(out[j].Created) })
	return out
}

// Snapshot returns the full state, secrets included. Used only by the export
// and cloud-sync paths, which immediately encrypt it.
func (s *Store) Snapshot() State {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var cp State
	b, _ := json.Marshal(s.state)
	_ = json.Unmarshal(b, &cp)
	return cp
}

// Restore replaces state wholesale (import / pair / cloud pull). Accounts are
// merged by ID so importing twice is idempotent and a device that added an
// account locally does not lose it.
func (s *Store) Restore(in State, replace bool) (added, updated int, err error) {
	err = s.Mutate(func(st *State) error {
		if replace {
			st.Accounts = nil
			st.Starred = nil
		}
		byID := map[string]*Account{}
		for _, a := range st.Accounts {
			byID[a.ID] = a
		}
		for _, in := range in.Accounts {
			if in == nil || in.ID == "" {
				continue
			}
			if cur, ok := byID[in.ID]; ok {
				*cur = *in
				updated++
				continue
			}
			in.Label = uniqueLabel(st.Accounts, in.Label)
			st.Accounts = append(st.Accounts, in)
			byID[in.ID] = in
			added++
		}

		// Settings and stars are unioned rather than overwritten.
		for k, v := range in.Settings.OAuthApps {
			if v.ClientID != "" {
				st.Settings.OAuthApps[k] = v
			}
		}
		if in.Settings.Strategy != "" {
			st.Settings.Strategy = in.Settings.Strategy
		}
		if len(in.Settings.ManualOrder) > 0 {
			st.Settings.ManualOrder = in.Settings.ManualOrder
		}
		seen := map[string]bool{}
		for _, k := range st.Starred {
			seen[k] = true
		}
		for _, k := range in.Starred {
			if !seen[k] {
				st.Starred = append(st.Starred, k)
				seen[k] = true
			}
		}
		sort.Strings(st.Starred)
		return nil
	})
	return added, updated, err
}

func uniqueLabel(existing []*Account, label string) string {
	if label == "" {
		label = "Drive"
	}
	taken := map[string]bool{}
	for _, a := range existing {
		taken[strings.ToLower(a.Label)] = true
	}
	if !taken[strings.ToLower(label)] {
		return label
	}
	for i := 2; ; i++ {
		cand := fmt.Sprintf("%s (%d)", label, i)
		if !taken[strings.ToLower(cand)] {
			return cand
		}
	}
}

// NewID returns a short random identifier.
func NewID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// atomicWrite writes to a sibling temp file then renames, so a crash can never
// leave a half-written state file behind.
func atomicWrite(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename succeeds

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, perm); err != nil {
		return err
	}
	// Windows refuses to rename onto an existing file.
	if _, err := os.Stat(path); err == nil {
		_ = os.Remove(path)
	}
	return os.Rename(tmpName, path)
}
