package alloc

import (
	"errors"
	"testing"

	"omnidrive/internal/store"
)

func acct(id string, used, total int64, weight int) *store.Account {
	return &store.Account{ID: id, Label: id, Enabled: true, Weight: weight, QuotaUsed: used, QuotaTotal: total}
}

func TestMostFree(t *testing.T) {
	accounts := []*store.Account{
		acct("a", 90, 100, 1), // 10 free
		acct("b", 10, 100, 1), // 90 free
		acct("c", 50, 100, 1), // 50 free
	}
	got, _, err := Choose(accounts, store.Settings{Strategy: store.StrategyMostFree}, 5, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "b" {
		t.Fatalf("want b (most free), got %s", got.ID)
	}
}

func TestLeastUsed(t *testing.T) {
	accounts := []*store.Account{acct("a", 90, 100, 1), acct("b", 10, 100, 1)}
	got, _, err := Choose(accounts, store.Settings{Strategy: store.StrategyLeastUsed}, 5, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "b" {
		t.Fatalf("want b, got %s", got.ID)
	}
}

func TestRoundRobinRotates(t *testing.T) {
	accounts := []*store.Account{acct("a", 0, 100, 1), acct("b", 0, 100, 1), acct("c", 0, 100, 1)}
	settings := store.Settings{Strategy: store.StrategyRoundRobin}

	cursor := 0
	var seen []string
	for i := 0; i < 6; i++ {
		got, next, err := Choose(accounts, settings, 1, cursor)
		if err != nil {
			t.Fatal(err)
		}
		seen = append(seen, got.ID)
		cursor = next
	}
	want := []string{"a", "b", "c", "a", "b", "c"}
	for i := range want {
		if seen[i] != want[i] {
			t.Fatalf("rotation %v, want %v", seen, want)
		}
	}
}

func TestWeightedFavoursHeavierAccount(t *testing.T) {
	accounts := []*store.Account{acct("a", 0, 100, 3), acct("b", 0, 100, 1)}
	settings := store.Settings{Strategy: store.StrategyWeighted}

	counts := map[string]int{}
	cursor := 0
	for i := 0; i < 40; i++ {
		got, next, err := Choose(accounts, settings, 1, cursor)
		if err != nil {
			t.Fatal(err)
		}
		counts[got.ID]++
		cursor = next
	}
	if counts["a"] != 30 || counts["b"] != 10 {
		t.Fatalf("weight 3:1 not honoured: %v", counts)
	}
}

func TestManualOrder(t *testing.T) {
	accounts := []*store.Account{acct("a", 0, 100, 1), acct("b", 0, 100, 1), acct("c", 0, 100, 1)}
	settings := store.Settings{Strategy: store.StrategyManual, ManualOrder: []string{"missing", "c", "a"}}

	got, _, err := Choose(accounts, settings, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "c" {
		t.Fatalf("want c (first present in manual order), got %s", got.ID)
	}
}

func TestDisabledAccountsAreSkipped(t *testing.T) {
	a := acct("a", 0, 100, 1)
	a.Enabled = false
	accounts := []*store.Account{a, acct("b", 99, 100, 1)}

	got, _, err := Choose(accounts, store.Settings{Strategy: store.StrategyMostFree}, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "b" {
		t.Fatalf("disabled account was chosen: %s", got.ID)
	}
}

// An account with no reported total (an S3 bucket, an unlimited plan) must
// stay usable rather than being treated as full.
func TestUnknownQuotaSortsAsSpacious(t *testing.T) {
	accounts := []*store.Account{acct("small", 0, 1000, 1), acct("unknown", 0, 0, 1)}
	got, _, err := Choose(accounts, store.Settings{Strategy: store.StrategyMostFree}, 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "unknown" {
		t.Fatalf("want unknown-quota account, got %s", got.ID)
	}
}

// If every drive looks too full, we still return one and let the provider
// deliver the authoritative error: cached quota is often stale.
func TestFallsBackWhenNothingFits(t *testing.T) {
	accounts := []*store.Account{acct("a", 100, 100, 1), acct("b", 100, 100, 1)}
	got, _, err := Choose(accounts, store.Settings{Strategy: store.StrategyMostFree}, 5_000_000, 0)
	if err != nil {
		t.Fatalf("expected a fallback choice, got error %v", err)
	}
	if got == nil {
		t.Fatal("nil account returned")
	}
}

func TestNoAccounts(t *testing.T) {
	_, _, err := Choose(nil, store.Settings{Strategy: store.StrategyMostFree}, 1, 0)
	if !errors.Is(err, ErrNoAccounts) {
		t.Fatalf("want ErrNoAccounts, got %v", err)
	}
}
