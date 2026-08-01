package server

import (
	"strings"

	"omnidrive/internal/provider"
	"omnidrive/internal/store"
)

// Account identity, so reconnecting a drive updates it instead of adding
// another copy of it.
//
// Every OAuth attempt that reached the end used to append a fresh account, so
// a few failed sign-ins followed by a successful one left the same Google
// Drive listed several times — each with its own tokens, all pointing at the
// same storage.

// identityKey returns a stable key describing *which storage* an account
// points at, ignoring anything that varies between connections (tokens,
// labels, timestamps). An empty string means "cannot tell", and the caller
// must not deduplicate on it.
func identityKey(kind provider.Kind, creds provider.Credentials) string {
	norm := func(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

	switch kind {
	case provider.KindTeraBox:
		// TeraBox has no email to key on, but every account carries a stable
		// numeric user id, which is what the driver records at sign-in.
		if uk := norm(creds["uk"]); uk != "" {
			return string(kind) + "|" + uk
		}
		return ""

	case provider.KindGoogleDrive, provider.KindOneDrive, provider.KindDropbox, provider.KindPCloud:
		// The signed-in identity. Without it we cannot tell two accounts of the
		// same provider apart, so we decline to guess.
		if name := norm(creds[provider.CredAccountName]); name != "" {
			return string(kind) + "|" + name
		}
		return ""

	case provider.KindWebDAV:
		return string(kind) + "|" + norm(strings.TrimRight(creds["url"], "/")) + "|" + norm(creds["username"])

	case provider.KindS3:
		return string(kind) + "|" + norm(creds["endpoint"]) + "|" +
			norm(creds["bucket"]) + "|" + norm(strings.Trim(creds["prefix"], "/"))

	case provider.KindLocal:
		return string(kind) + "|" + norm(strings.TrimRight(creds["root"], `/\`))
	}
	return ""
}

// findByIdentity locates an existing account pointing at the same storage.
func (s *Server) findByIdentity(kind provider.Kind, creds provider.Credentials) *store.Account {
	key := identityKey(kind, creds)
	if key == "" {
		return nil
	}
	for _, a := range s.st.Accounts() {
		if a.Kind != kind {
			continue
		}
		if identityKey(a.Kind, a.Creds) == key {
			return a
		}
	}
	return nil
}

// upsertAccount adds a drive, or refreshes the credentials of the one already
// pointing at the same storage. Returns the account and whether it is new.
func (s *Server) upsertAccount(kind provider.Kind, label string, creds provider.Credentials) (*store.Account, bool, error) {
	if existing := s.findByIdentity(kind, creds); existing != nil {
		err := s.st.Mutate(func(st *store.State) error {
			for _, a := range st.Accounts {
				if a.ID != existing.ID {
					continue
				}
				a.Creds = creds
				a.Enabled = true
				// Keep a name the user chose; replace an automatic one.
				if label != "" && looksAutoLabel(a.Label, a.Kind) {
					a.Label = label
				}
				return nil
			}
			return nil
		})
		if err != nil {
			return nil, false, err
		}
		// The cached driver still holds the old credentials.
		s.drivers.Delete(existing.ID)

		refreshed, _ := s.st.Account(existing.ID)
		return refreshed, false, nil
	}

	acc, err := s.st.AddAccount(kind, label, creds)
	return acc, true, err
}

// DedupeAccounts merges drives that point at the same storage, keeping the
// most recently connected credentials. Existing installs accumulated these
// before connect became idempotent, so this repairs them on start.
//
// Returns the labels of the copies removed.
func (s *Server) DedupeAccounts() []string {
	var removed []string
	seen := map[string]*store.Account{}

	for _, a := range s.st.Accounts() {
		key := identityKey(a.Kind, a.Creds)
		if key == "" {
			continue // cannot tell them apart; leave well alone
		}
		first, ok := seen[key]
		if !ok {
			seen[key] = a
			continue
		}
		// Keep whichever was connected later: its tokens are the fresh ones.
		keep, drop := first, a
		if a.CreatedAt.After(first.CreatedAt) {
			keep, drop = a, first
			seen[key] = a
		}
		// Carry over a name the user chose before discarding the copy.
		if looksAutoLabel(keep.Label, keep.Kind) && !looksAutoLabel(drop.Label, drop.Kind) {
			_ = s.st.Mutate(func(st *store.State) error {
				for _, x := range st.Accounts {
					if x.ID == keep.ID {
						x.Label = drop.Label
					}
				}
				return nil
			})
		}
		if err := s.st.RemoveAccount(drop.ID); err == nil {
			s.drivers.Delete(drop.ID)
			removed = append(removed, drop.Label)
		}
	}
	return removed
}

// looksAutoLabel reports whether a label was generated rather than chosen, so
// reconnecting never overwrites a name the user typed.
func looksAutoLabel(label string, kind provider.Kind) bool {
	if strings.Contains(label, "@") {
		return true
	}
	for _, d := range provider.Descriptors() {
		if d.Kind == kind && strings.HasPrefix(label, d.Label) {
			return true
		}
	}
	return false
}
