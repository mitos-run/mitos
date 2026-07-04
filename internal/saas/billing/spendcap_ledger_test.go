package billing

import (
	"context"
	"testing"
	"time"
)

// seedLedger appends the given entries, failing the test on error.
func seedLedger(t *testing.T, l CreditLedger, entries ...LedgerEntry) {
	t.Helper()
	for _, e := range entries {
		if err := l.Append(context.Background(), e); err != nil {
			t.Fatalf("Append %s: %v", e.Key, err)
		}
	}
}

// TestEnforceSpendCapFromLedgerSuspendsOnBreach asserts the ledger-derived
// period spend (usage drawdowns in the current calendar month, UTC) drives the
// spend cap: below the hard cap nothing fires; once this month's drawdowns
// cross it the org is suspended through the Suspender seam with a manual hold.
// Prior-month drawdowns and credits (top-ups) never count as spend.
func TestEnforceSpendCapFromLedgerSuspendsOnBreach(t *testing.T) {
	ctx := context.Background()
	ledger := NewMemCreditLedger()
	sus := &recordingSuspender{}
	svc := NewService(Config{Ledger: ledger, Suspend: sus, Now: fixedNow}) // 2026-06-21.
	if err := svc.SetSpendCap(ctx, SpendCap{OrgID: "org1", HardCap: 400}); err != nil {
		t.Fatalf("SetSpendCap: %v", err)
	}
	seedLedger(t, ledger,
		// This month: 300 cents drawn down.
		LedgerEntry{OrgID: "org1", Kind: KindUsageDrawdown, Amount: -300, Key: "dd-june-1", At: time.Date(2026, 6, 10, 0, 0, 0, 0, time.UTC)},
		// Previous month: excluded from the period.
		LedgerEntry{OrgID: "org1", Kind: KindUsageDrawdown, Amount: -1000, Key: "dd-may", At: time.Date(2026, 5, 20, 0, 0, 0, 0, time.UTC)},
		// Credit: never spend.
		LedgerEntry{OrgID: "org1", Kind: KindTopUp, Amount: 5000, Key: "topup-1", At: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)},
	)

	suspended, err := svc.EnforceSpendCapFromLedger(ctx, "org1")
	if err != nil || suspended {
		t.Fatalf("below-cap EnforceSpendCapFromLedger = %t, %v; want false, nil (period spend 300 < cap 400)", suspended, err)
	}
	if len(sus.calls) != 0 {
		t.Fatalf("suspender fired below the cap: %+v", sus.calls)
	}

	// Another 200 cents this month crosses the 400-cent hard cap.
	seedLedger(t, ledger,
		LedgerEntry{OrgID: "org1", Kind: KindUsageDrawdown, Amount: -200, Key: "dd-june-2", At: time.Date(2026, 6, 20, 0, 0, 0, 0, time.UTC)},
	)
	suspended, err = svc.EnforceSpendCapFromLedger(ctx, "org1")
	if err != nil || !suspended {
		t.Fatalf("at-cap EnforceSpendCapFromLedger = %t, %v; want true, nil (period spend 500 >= cap 400)", suspended, err)
	}
	if len(sus.calls) != 1 {
		t.Fatalf("suspender calls = %d, want 1", len(sus.calls))
	}
	call := sus.calls[0]
	if call.orgID != "org1" || call.reason != "spend_cap" {
		t.Errorf("suspend call = %+v, want org1/spend_cap", call)
	}
	// No manual hold: a paid top-up is the lift lever for a spend-cap
	// suspension, so the automated breach path must not require a human.
	if call.manualHold {
		t.Error("automated spend-cap suspend carries a manual hold; a top-up could then never lift it")
	}
}

// TestEnforceSpendCapFromLedgerTopUpResetsWindow asserts a paid top-up resets
// the spend window: period spend counts only drawdowns settled AFTER the
// latest in-month top-up, so an org that breached, paid, and was lifted is not
// re-suspended on the very next cycle for the same pre-payment spend; only
// burning past the cap AGAIN re-suspends.
func TestEnforceSpendCapFromLedgerTopUpResetsWindow(t *testing.T) {
	ctx := context.Background()
	ledger := NewMemCreditLedger()
	sus := &recordingSuspender{}
	svc := NewService(Config{Ledger: ledger, Suspend: sus, Now: fixedNow}) // 2026-06-21.
	if err := svc.SetSpendCap(ctx, SpendCap{OrgID: "org1", HardCap: 400}); err != nil {
		t.Fatalf("SetSpendCap: %v", err)
	}
	seedLedger(t, ledger,
		// Pre-payment spend this month: 500, over the 400 cap.
		LedgerEntry{OrgID: "org1", Kind: KindUsageDrawdown, Amount: -500, Key: "dd-early", At: time.Date(2026, 6, 10, 0, 0, 0, 0, time.UTC)},
		// The org paid on June 15: the window resets here.
		LedgerEntry{OrgID: "org1", Kind: KindTopUp, Amount: 2000, Key: "topup-mid", At: time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC)},
		// Post-payment spend: 100, under the cap.
		LedgerEntry{OrgID: "org1", Kind: KindUsageDrawdown, Amount: -100, Key: "dd-late", At: time.Date(2026, 6, 18, 0, 0, 0, 0, time.UTC)},
	)
	suspended, err := svc.EnforceSpendCapFromLedger(ctx, "org1")
	if err != nil || suspended {
		t.Fatalf("post-top-up EnforceSpendCapFromLedger = %t, %v; want false, nil (window reset at the payment)", suspended, err)
	}
	if len(sus.calls) != 0 {
		t.Fatalf("suspender fired for pre-payment spend after a top-up: %+v", sus.calls)
	}

	// Burning past the cap again after the payment re-suspends.
	seedLedger(t, ledger,
		LedgerEntry{OrgID: "org1", Kind: KindUsageDrawdown, Amount: -350, Key: "dd-late-2", At: time.Date(2026, 6, 20, 0, 0, 0, 0, time.UTC)},
	)
	suspended, err = svc.EnforceSpendCapFromLedger(ctx, "org1")
	if err != nil || !suspended {
		t.Fatalf("re-breach EnforceSpendCapFromLedger = %t, %v; want true, nil (450 since the payment >= cap 400)", suspended, err)
	}
}

// TestMemEntriesSince pins the scoped ledger read the spend-cap evaluation
// uses: only entries at or after the bound are returned, so the per-cycle read
// is bounded by the month, not the org's lifetime history.
func TestMemEntriesSince(t *testing.T) {
	ctx := context.Background()
	ledger := NewMemCreditLedger()
	seedLedger(t, ledger,
		LedgerEntry{OrgID: "org1", Kind: KindUsageDrawdown, Amount: -10, Key: "old", At: time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)},
		LedgerEntry{OrgID: "org1", Kind: KindUsageDrawdown, Amount: -20, Key: "new", At: time.Date(2026, 6, 2, 0, 0, 0, 0, time.UTC)},
	)
	got, err := ledger.EntriesSince(ctx, "org1", time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("EntriesSince: %v", err)
	}
	if len(got) != 1 || got[0].Key != "new" {
		t.Fatalf("EntriesSince = %+v, want only the in-window entry", got)
	}
}

// TestEnforceSpendCapFromLedgerNoCapIsNoOp asserts an org with no configured
// cap short-circuits without touching the ledger or the suspender: the cap is
// opt-in, and an uncapped org must never pay a per-cycle ledger scan.
func TestEnforceSpendCapFromLedgerNoCapIsNoOp(t *testing.T) {
	sus := &recordingSuspender{}
	// errLedgerEntries fails on Entries, so any read proves the short-circuit
	// was skipped.
	svc := NewService(Config{Ledger: errEntriesLedger{NewMemCreditLedger()}, Suspend: sus, Now: fixedNow})
	suspended, err := svc.EnforceSpendCapFromLedger(context.Background(), "org-uncapped")
	if err != nil || suspended {
		t.Fatalf("no-cap EnforceSpendCapFromLedger = %t, %v; want false, nil without a ledger read", suspended, err)
	}
	if len(sus.calls) != 0 {
		t.Errorf("suspender fired for an uncapped org: %+v", sus.calls)
	}
}

// errEntriesLedger fails Entries; every other method delegates to the mem
// ledger and is unused in the no-cap short-circuit test.
type errEntriesLedger struct{ *MemCreditLedger }

func (errEntriesLedger) Entries(context.Context, string) ([]LedgerEntry, error) {
	return nil, context.DeadlineExceeded
}
