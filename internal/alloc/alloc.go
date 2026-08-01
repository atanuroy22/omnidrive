// Package alloc decides which connected drive receives a new upload when the
// user does not pick one explicitly. The strategy names match upstream
// OmniCloud so the concept transfers between the two.
package alloc

import (
	"errors"
	"sort"

	"omnidrive/internal/store"
)

// ErrNoAccounts means nothing is connected and enabled.
var ErrNoAccounts = errors.New("no enabled accounts to upload into")

// Choose picks a destination account for a file of the given size.
//
// cursor is the persisted round-robin position; the returned cursor should be
// written back so the rotation survives a restart.
func Choose(accounts []*store.Account, settings store.Settings, size int64, cursor int) (*store.Account, int, error) {
	eligible := filter(accounts, size)
	if len(eligible) == 0 {
		// Fall back to ignoring the size check: better to attempt the upload
		// and surface the provider's own error than to refuse locally on the
		// basis of a possibly stale quota reading.
		eligible = filter(accounts, 0)
	}
	if len(eligible) == 0 {
		return nil, cursor, ErrNoAccounts
	}

	switch settings.Strategy {
	case store.StrategyRoundRobin:
		idx := ((cursor % len(eligible)) + len(eligible)) % len(eligible)
		return eligible[idx], cursor + 1, nil

	case store.StrategyWeighted:
		// Expand each account into `weight` slots and rotate through them, so
		// a drive with weight 3 receives three times as many files.
		var slots []*store.Account
		for _, a := range eligible {
			w := a.Weight
			if w < 1 {
				w = 1
			}
			if w > 100 {
				w = 100
			}
			for i := 0; i < w; i++ {
				slots = append(slots, a)
			}
		}
		idx := ((cursor % len(slots)) + len(slots)) % len(slots)
		return slots[idx], cursor + 1, nil

	case store.StrategyLeastUsed:
		sort.SliceStable(eligible, func(i, j int) bool {
			return eligible[i].QuotaUsed < eligible[j].QuotaUsed
		})
		return eligible[0], cursor, nil

	case store.StrategyManual:
		if a := firstInOrder(eligible, settings.ManualOrder); a != nil {
			return a, cursor, nil
		}
		return eligible[0], cursor, nil

	case store.StrategyMostFree:
		fallthrough
	default:
		sort.SliceStable(eligible, func(i, j int) bool {
			return free(eligible[i]) > free(eligible[j])
		})
		return eligible[0], cursor, nil
	}
}

// filter keeps enabled accounts that plausibly have room for size bytes.
func filter(accounts []*store.Account, size int64) []*store.Account {
	var out []*store.Account
	for _, a := range accounts {
		if !a.Enabled {
			continue
		}
		if size > 0 && a.QuotaTotal > 0 && free(a) < size {
			continue
		}
		out = append(out, a)
	}
	return out
}

// free returns remaining bytes. Accounts with an unknown total (S3 buckets,
// unlimited plans) sort as effectively infinite so they stay usable.
func free(a *store.Account) int64 {
	if a.QuotaTotal <= 0 {
		const unknownFree = int64(1) << 50 // 1 PiB
		return unknownFree
	}
	f := a.QuotaTotal - a.QuotaUsed
	if f < 0 {
		return 0
	}
	return f
}

func firstInOrder(accounts []*store.Account, order []string) *store.Account {
	byID := map[string]*store.Account{}
	for _, a := range accounts {
		byID[a.ID] = a
	}
	for _, id := range order {
		if a, ok := byID[id]; ok {
			return a
		}
	}
	return nil
}

// Strategies lists the selectable strategies with human labels, for the UI.
func Strategies() []map[string]string {
	return []map[string]string{
		{"value": store.StrategyMostFree, "label": "Most free space"},
		{"value": store.StrategyLeastUsed, "label": "Least used"},
		{"value": store.StrategyRoundRobin, "label": "Round robin"},
		{"value": store.StrategyWeighted, "label": "Weighted round robin"},
		{"value": store.StrategyManual, "label": "Manual priority order"},
	}
}
