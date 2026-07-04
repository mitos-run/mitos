package pgstore_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"mitos.run/mitos/internal/saas/billing"
	"mitos.run/mitos/internal/saas/pgstore"
)

func TestPgCreditLedger(t *testing.T) {
	dsn := testDSN(t)
	pg, err := pgstore.Open(context.Background(), dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(pg.Close)
	truncateTables(t, dsn, "credit_ledger")
	l := pgstore.NewPgCreditLedger(pg.Pool())
	ctx := context.Background()
	now := time.Unix(1700000000, 0).UTC()

	mustAppend := func(e billing.LedgerEntry) {
		t.Helper()
		if err := l.Append(ctx, e); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	mustAppend(billing.LedgerEntry{OrgID: "o1", Kind: billing.KindSignupCredit, Amount: billing.USD(5), Key: "signup:o1", At: now})
	mustAppend(billing.LedgerEntry{OrgID: "o1", Kind: billing.KindUsageDrawdown, Amount: -billing.USD(2), Key: "u1", At: now})

	bal, err := l.Balance(ctx, "o1")
	if err != nil {
		t.Fatalf("balance: %v", err)
	}
	if bal != billing.USD(3) {
		t.Fatalf("balance = %d, want %d", bal, billing.USD(3))
	}

	// Duplicate non-empty key is rejected and does not change the balance.
	dupErr := l.Append(ctx, billing.LedgerEntry{OrgID: "o1", Kind: billing.KindSignupCredit, Amount: billing.USD(5), Key: "signup:o1", At: now})
	if !errors.Is(dupErr, billing.ErrDuplicateEntry) {
		t.Fatalf("duplicate key err = %v, want ErrDuplicateEntry", dupErr)
	}
	bal2, _ := l.Balance(ctx, "o1")
	if bal2 != billing.USD(3) {
		t.Fatalf("balance after dup = %d, want unchanged %d", bal2, billing.USD(3))
	}

	entries, err := l.Entries(ctx, "o1")
	if err != nil {
		t.Fatalf("entries: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(entries))
	}
}

// TestPgCreditLedgerRemainder covers the durable drawdown remainder (issue
// #662, migration 0010): the default remainder is zero, SettleWindow commits
// the ledger entry and the remainder atomically, a replayed window leaves BOTH
// untouched, a negative carry (the round-half-up prepaid sub-cent) round-trips,
// and the remainder survives a restart (a second pgstore.Open over the same
// database).
func TestPgCreditLedgerRemainder(t *testing.T) {
	dsn := testDSN(t)
	pg, err := pgstore.Open(context.Background(), dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(pg.Close)
	truncateTables(t, dsn, "credit_ledger", "drawdown_remainders", "processed_usage_windows")
	l := pgstore.NewPgCreditLedger(pg.Pool())
	ctx := context.Background()
	now := time.Unix(1700000000, 0).UTC()

	// Unknown org: remainder defaults to zero.
	rem, err := l.Remainder(ctx, "o1")
	if err != nil {
		t.Fatalf("remainder (fresh): %v", err)
	}
	if rem != 0 {
		t.Fatalf("fresh remainder = %d, want 0", rem)
	}

	// The combined write lands the entry, the remainder, and the marker.
	w1 := billing.ProcessedWindow{OrgID: "o1", SandboxID: "sb1", Window: now}
	e := billing.LedgerEntry{OrgID: "o1", Kind: billing.KindUsageDrawdown, Amount: -1, Key: w1.Key(), At: now, Note: "usage drawdown"}
	if err := l.SettleWindow(ctx, e, 231, w1); err != nil {
		t.Fatalf("settle window: %v", err)
	}
	rem, _ = l.Remainder(ctx, "o1")
	if rem != 231 {
		t.Fatalf("remainder = %d, want 231", rem)
	}
	bal, _ := l.Balance(ctx, "o1")
	if bal != -1 {
		t.Fatalf("balance = %d, want -1 (the appended debit)", int64(bal))
	}

	// A replayed window is atomic: ErrDuplicateEntry, and NEITHER the entries
	// nor the remainder move (a replayed drawdown cannot double-count the carry).
	if err := l.SettleWindow(ctx, e, 999, w1); !errors.Is(err, billing.ErrDuplicateEntry) {
		t.Fatalf("duplicate err = %v, want ErrDuplicateEntry", err)
	}
	rem, _ = l.Remainder(ctx, "o1")
	if rem != 231 {
		t.Fatalf("remainder after duplicate = %d, want 231 (untouched)", rem)
	}
	entries, _ := l.Entries(ctx, "o1")
	if len(entries) != 1 {
		t.Fatalf("entries after duplicate = %d, want 1", len(entries))
	}

	// A negative carry (round-half-up prepaid the sub-cent) round-trips.
	w2 := billing.ProcessedWindow{OrgID: "o1", SandboxID: "sb1", Window: now.Add(time.Minute)}
	e2 := billing.LedgerEntry{OrgID: "o1", Kind: billing.KindUsageDrawdown, Amount: -1, Key: w2.Key(), At: now, Note: "usage drawdown"}
	if err := l.SettleWindow(ctx, e2, -461, w2); err != nil {
		t.Fatalf("settle negative remainder: %v", err)
	}
	rem, _ = l.Remainder(ctx, "o1")
	if rem != -461 {
		t.Fatalf("remainder = %d, want -461", rem)
	}

	// Restart survival: a fresh Open (the migration chain re-runs idempotently)
	// still reads the carried remainder.
	pg2, err := pgstore.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(pg2.Close)
	l2 := pgstore.NewPgCreditLedger(pg2.Pool())
	rem, err = l2.Remainder(ctx, "o1")
	if err != nil {
		t.Fatalf("remainder after reopen: %v", err)
	}
	if rem != -461 {
		t.Fatalf("remainder after restart = %d, want -461 (must be durable)", rem)
	}
}

// TestPgProcessedWindows covers migration 0011 end to end on a real Postgres:
// a zero-amount settle writes the marker and the remainder but NO ledger row,
// a replayed window rolls the whole transaction back, a window settled before
// the marker table (a keyed ledger row from the pre-#672 deploy) both blocks
// a direct re-settle AND appears in the skip set, and pruning removes only
// markers older than the horizon.
func TestPgProcessedWindows(t *testing.T) {
	dsn := testDSN(t)
	pg, err := pgstore.Open(context.Background(), dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(pg.Close)
	truncateTables(t, dsn, "credit_ledger", "drawdown_remainders", "processed_usage_windows")
	l := pgstore.NewPgCreditLedger(pg.Pool())
	ctx := context.Background()
	now := time.Unix(1700000000, 0).UTC()

	// The migration chain through 0011 is recorded.
	var applied bool
	if err := pg.Pool().QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = '0011_processed_usage_windows.sql')`).Scan(&applied); err != nil {
		t.Fatalf("read schema_migrations: %v", err)
	}
	if !applied {
		t.Fatalf("migration 0011_processed_usage_windows.sql not recorded as applied")
	}

	// Zero-amount settle: marker + remainder land, the customer-visible ledger
	// stays clean (issue #672: no more zero-amount usage_drawdown rows).
	wZero := billing.ProcessedWindow{OrgID: "o1", SandboxID: "sb1", Window: now}
	eZero := billing.LedgerEntry{OrgID: "o1", Kind: billing.KindUsageDrawdown, Amount: 0, Key: wZero.Key(), At: now, Note: "usage drawdown"}
	if err := l.SettleWindow(ctx, eZero, 77, wZero); err != nil {
		t.Fatalf("zero-amount settle: %v", err)
	}
	entries, _ := l.Entries(ctx, "o1")
	if len(entries) != 0 {
		t.Fatalf("zero-amount settle wrote %d ledger rows, want 0", len(entries))
	}
	rem, _ := l.Remainder(ctx, "o1")
	if rem != 77 {
		t.Fatalf("remainder = %d, want 77", rem)
	}
	// The marker still deduplicates: a replay changes nothing.
	if err := l.SettleWindow(ctx, eZero, 154, wZero); !errors.Is(err, billing.ErrDuplicateEntry) {
		t.Fatalf("replayed zero-amount settle err = %v, want ErrDuplicateEntry", err)
	}
	rem, _ = l.Remainder(ctx, "o1")
	if rem != 77 {
		t.Fatalf("remainder after replay = %d, want 77 (untouched)", rem)
	}

	// Transition compatibility: a window settled pre-#672 exists only as a
	// keyed ledger row. A re-settle must be rejected atomically (no marker
	// half-landed) and the skip set must report it settled.
	wLegacy := billing.ProcessedWindow{OrgID: "o1", SandboxID: "sb1", Window: now.Add(time.Minute)}
	if err := l.Append(ctx, billing.LedgerEntry{OrgID: "o1", Kind: billing.KindUsageDrawdown, Amount: -2, Key: wLegacy.Key(), At: now.Add(time.Minute), Note: "usage drawdown"}); err != nil {
		t.Fatalf("legacy append: %v", err)
	}
	eLegacy := billing.LedgerEntry{OrgID: "o1", Kind: billing.KindUsageDrawdown, Amount: -2, Key: wLegacy.Key(), At: now.Add(2 * time.Minute), Note: "usage drawdown"}
	if err := l.SettleWindow(ctx, eLegacy, 300, wLegacy); !errors.Is(err, billing.ErrDuplicateEntry) {
		t.Fatalf("settle over legacy key err = %v, want ErrDuplicateEntry", err)
	}
	rem, _ = l.Remainder(ctx, "o1")
	if rem != 77 {
		t.Fatalf("remainder after rejected legacy settle = %d, want 77 (untouched)", rem)
	}
	var markers int
	if err := pg.Pool().QueryRow(ctx, `SELECT COUNT(*) FROM processed_usage_windows WHERE org_id = 'o1'`).Scan(&markers); err != nil {
		t.Fatalf("count markers: %v", err)
	}
	if markers != 1 {
		t.Fatalf("markers = %d, want 1 (the rejected settle must not half-land its marker)", markers)
	}

	// The skip set unions both mechanisms, scoped to the org and the since
	// bound; another org's settles never leak in.
	wOther := billing.ProcessedWindow{OrgID: "o2", SandboxID: "sb9", Window: now}
	if err := l.SettleWindow(ctx, billing.LedgerEntry{OrgID: "o2", Kind: billing.KindUsageDrawdown, Amount: 0, Key: wOther.Key(), At: now}, 0, wOther); err != nil {
		t.Fatalf("other-org settle: %v", err)
	}
	keys, err := l.SettledWindowKeys(ctx, "o1", now.Add(-time.Hour))
	if err != nil {
		t.Fatalf("settled keys: %v", err)
	}
	if !keys[wZero.Key()] {
		t.Errorf("skip set missing the marker key %q", wZero.Key())
	}
	if !keys[wLegacy.Key()] {
		t.Errorf("skip set missing the legacy ledger key %q", wLegacy.Key())
	}
	if keys[wOther.Key()] {
		t.Errorf("skip set leaked another org's key %q", wOther.Key())
	}
	if len(keys) != 2 {
		t.Errorf("skip set size = %d, want 2", len(keys))
	}
	// A since bound after everything yields an empty set.
	none, err := l.SettledWindowKeys(ctx, "o1", now.Add(time.Hour))
	if err != nil {
		t.Fatalf("settled keys (future since): %v", err)
	}
	if len(none) != 0 {
		t.Errorf("skip set with future since = %d keys, want 0", len(none))
	}

	// Pruning removes only markers whose window predates the horizon.
	wOld := billing.ProcessedWindow{OrgID: "o1", SandboxID: "sb1", Window: now.Add(-3 * time.Hour)}
	if err := l.SettleWindow(ctx, billing.LedgerEntry{OrgID: "o1", Kind: billing.KindUsageDrawdown, Amount: 0, Key: wOld.Key(), At: now}, 77, wOld); err != nil {
		t.Fatalf("old-window settle: %v", err)
	}
	pruned, err := l.PruneProcessedWindows(ctx, now.Add(-2*time.Hour))
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if pruned != 1 {
		t.Fatalf("pruned = %d, want 1 (only the aged marker)", pruned)
	}
	keys, _ = l.SettledWindowKeys(ctx, "o1", time.Time{})
	if keys[wOld.Key()] {
		t.Errorf("pruned marker %q still in the skip set", wOld.Key())
	}
	if !keys[wZero.Key()] {
		t.Errorf("in-horizon marker %q was pruned", wZero.Key())
	}
}

// TestPgCreditLedgerEntriesSince pins the month-scoped read the spend-cap
// evaluation uses (billing.ScopedLedgerReader, issue #615): only entries at or
// after the bound return, so the per-cycle read is bounded by the month, not
// the org's lifetime history. Requires MITOS_TEST_DATABASE_DSN (skips
// otherwise).
func TestPgCreditLedgerEntriesSince(t *testing.T) {
	dsn := testDSN(t)
	pg, err := pgstore.Open(context.Background(), dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(pg.Close)
	truncateTables(t, dsn, "credit_ledger")
	l := pgstore.NewPgCreditLedger(pg.Pool())
	ctx := context.Background()
	old := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	cut := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	if err := l.Append(ctx, billing.LedgerEntry{OrgID: "o1", Kind: billing.KindUsageDrawdown, Amount: -10, Key: "old", At: old}); err != nil {
		t.Fatalf("append old: %v", err)
	}
	if err := l.Append(ctx, billing.LedgerEntry{OrgID: "o1", Kind: billing.KindUsageDrawdown, Amount: -20, Key: "new", At: cut.Add(24 * time.Hour)}); err != nil {
		t.Fatalf("append new: %v", err)
	}
	// Another org's in-window entry never leaks in.
	if err := l.Append(ctx, billing.LedgerEntry{OrgID: "o2", Kind: billing.KindUsageDrawdown, Amount: -30, Key: "other", At: cut.Add(24 * time.Hour)}); err != nil {
		t.Fatalf("append other: %v", err)
	}
	got, err := l.EntriesSince(ctx, "o1", cut)
	if err != nil {
		t.Fatalf("EntriesSince: %v", err)
	}
	if len(got) != 1 || got[0].Key != "new" {
		t.Fatalf("EntriesSince = %+v, want only o1's in-window entry", got)
	}
}
