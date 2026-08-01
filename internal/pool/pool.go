// Package pool presents every connected drive as one combined storage space.
//
// The idea it implements: you should not have to know, or care, which account
// a file lives on. The pool shows a single folder tree merged across all
// drives and a single capacity figure that is the sum of them; when something
// is written, the allocator quietly picks whichever drive has room.
//
// Folders in the pool are identified by *path*, because the same folder can
// exist on several drives at once and must appear as one. Files keep their
// real account and provider ID, so downloading, renaming or deleting a pooled
// file needs no translation — it addresses the actual drive directly.
package pool

import (
	"context"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"omnidrive/internal/provider"
	"omnidrive/internal/store"
)

// ID is the account identifier the pool answers to.
const ID = "pool"

// Label is what the combined drive is called in the UI.
//
// Only cloud accounts are pooled. Phone storage and SD cards are physical
// places the user already thinks of separately — merging them into the cloud
// would mean a file "in the cloud" might actually be sitting on the phone,
// which is the opposite of useful.
const Label = "Cloud"

// Member is one drive taking part in the pool.
type Member struct {
	Account *store.Account
	Driver  provider.Driver
}

// Pool merges a set of drives into one namespace.
type Pool struct {
	members []Member

	mu    sync.Mutex
	paths map[string]string // "accountID\x00path" -> provider folder ID
	seen  time.Time
}

// New builds a pool over the given members.
func New(members []Member) *Pool {
	return &Pool{members: members, paths: map[string]string{}, seen: time.Now()}
}

// Members exposes the participating drives.
func (p *Pool) Members() []Member { return p.members }

// Quota is the sum of every member's capacity: the single big number.
//
// Drives that do not report a total (an S3 bucket, an unlimited plan) still
// contribute what they are using, but cannot contribute capacity — so the
// total is a floor, never an overstatement.
func (p *Pool) Quota(ctx context.Context) provider.Quota {
	var total provider.Quota
	for _, m := range p.members {
		total.Used += m.Account.QuotaUsed
		if m.Account.QuotaTotal > 0 {
			total.Total += m.Account.QuotaTotal
		}
	}
	return total
}

// cacheKey identifies a resolved path on one account.
func cacheKey(accountID, dir string) string { return accountID + "\x00" + dir }

func (p *Pool) cached(accountID, dir string) (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	// Folder IDs are stable, but a folder deleted elsewhere would linger, so
	// the map is dropped periodically rather than kept forever.
	if time.Since(p.seen) > 2*time.Minute {
		p.paths = map[string]string{}
		p.seen = time.Now()
		return "", false
	}
	id, ok := p.paths[cacheKey(accountID, dir)]
	return id, ok
}

func (p *Pool) remember(accountID, dir, id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.paths[cacheKey(accountID, dir)] = id
}

// resolve walks a pool path down one drive, returning that drive's folder ID.
// ok is false when the drive simply does not have this folder, which is
// normal and not an error.
func (p *Pool) resolve(ctx context.Context, m Member, dir string) (id string, ok bool, err error) {
	dir = cleanPath(dir)
	if dir == "" {
		return "", true, nil // the drive's own root
	}
	if id, hit := p.cached(m.Account.ID, dir); hit {
		return id, id != "\x00missing", nil
	}

	current := ""
	for _, segment := range strings.Split(dir, "/") {
		entries, listErr := m.Driver.List(ctx, current)
		if listErr != nil {
			return "", false, listErr
		}
		found := ""
		for _, e := range entries {
			if e.IsDir && strings.EqualFold(e.Name, segment) {
				found = e.ID
				break
			}
		}
		if found == "" {
			// Record the absence too: without this, every listing re-walks
			// the whole tree on every drive that lacks the folder.
			p.remember(m.Account.ID, dir, "\x00missing")
			return "", false, nil
		}
		current = found
	}
	p.remember(m.Account.ID, dir, current)
	return current, true, nil
}

// ResolveFor exposes path resolution for one member, so callers can act on the
// real folder behind a pooled path.
func (p *Pool) ResolveFor(ctx context.Context, m Member, dir string) (string, bool, error) {
	return p.resolve(ctx, m, dir)
}

// EnsurePath creates a pool path on one drive, making intermediate folders as
// needed, and returns the drive's folder ID for it.
func (p *Pool) EnsurePath(ctx context.Context, m Member, dir string) (string, error) {
	dir = cleanPath(dir)
	if dir == "" {
		return "", nil
	}
	current := ""
	built := ""
	for _, segment := range strings.Split(dir, "/") {
		entries, err := m.Driver.List(ctx, current)
		if err != nil {
			return "", err
		}
		next := ""
		for _, e := range entries {
			if e.IsDir && strings.EqualFold(e.Name, segment) {
				next = e.ID
				break
			}
		}
		if next == "" {
			created, err := m.Driver.Mkdir(ctx, current, segment)
			if err != nil {
				return "", fmt.Errorf("create %q on %s: %w", segment, m.Account.Label, err)
			}
			next = created.ID
		}
		current = next
		built = path.Join(built, segment)
		p.remember(m.Account.ID, built, current)
	}
	return current, nil
}

// List merges the contents of dir across every drive.
//
// Folders are unioned by name, so a folder present on three drives appears
// once. Files keep their own identity; if the same name genuinely exists on
// two drives, both are listed rather than one being silently hidden.
func (p *Pool) List(ctx context.Context, dir string) ([]provider.File, error) {
	dir = cleanPath(dir)

	type result struct {
		files []provider.File
		err   error
		m     Member
	}
	results := make([]result, len(p.members))

	var wg sync.WaitGroup
	for i, m := range p.members {
		wg.Add(1)
		go func(i int, m Member) {
			defer wg.Done()
			id, ok, err := p.resolve(ctx, m, dir)
			if err != nil {
				results[i] = result{err: err, m: m}
				return
			}
			if !ok {
				return // this drive has no such folder
			}
			files, err := m.Driver.List(ctx, id)
			results[i] = result{files: files, err: err, m: m}
		}(i, m)
	}
	wg.Wait()

	var (
		out      []provider.File
		folders  = map[string]bool{}
		problems []string
		anyOK    bool
	)
	for _, r := range results {
		if r.err != nil {
			if r.m.Account != nil {
				problems = append(problems, r.m.Account.Label+": "+r.err.Error())
			}
			continue
		}
		anyOK = true
		for _, f := range r.files {
			f.AccountID = r.m.Account.ID
			f.AccountLabel = r.m.Account.Label

			if f.IsDir {
				key := strings.ToLower(f.Name)
				if folders[key] {
					continue // already contributed by another drive
				}
				folders[key] = true
				// A pooled folder is addressed by path, not by any one
				// drive's ID, because it may live on several.
				f.ID = path.Join(dir, f.Name)
				f.Path = f.ID
				f.AccountID = ID
				f.AccountLabel = Label
			} else {
				f.Path = path.Join(dir, f.Name)
			}
			out = append(out, f)
		}
	}

	if !anyOK && len(problems) > 0 {
		return nil, errors.New(strings.Join(problems, "; "))
	}
	provider.SortFiles(out)
	return out, nil
}

// CleanPath normalises a pool path: no leading or trailing slash, no dot
// segments, forward slashes only.
func CleanPath(p string) string { return cleanPath(p) }

func cleanPath(p string) string {
	p = strings.ReplaceAll(strings.TrimSpace(p), "\\", "/")
	p = path.Clean("/" + p)
	return strings.Trim(p, "/")
}

// SortMembers orders drives most-free first, which is the order the allocator
// prefers and makes listings deterministic.
func SortMembers(m []Member) {
	sort.SliceStable(m, func(i, j int) bool {
		return free(m[i].Account) > free(m[j].Account)
	})
}

func free(a *store.Account) int64 {
	if a.QuotaTotal <= 0 {
		return 1 << 50
	}
	if f := a.QuotaTotal - a.QuotaUsed; f > 0 {
		return f
	}
	return 0
}
