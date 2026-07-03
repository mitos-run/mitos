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
// #662, migration 0010): the default remainder is zero, AppendWithRemainder
// commits the ledger entry and the remainder atomically, a duplicate
// idempotency key leaves BOTH untouched, a negative carry (the round-half-up
// prepaid sub-cent) round-trips, and the remainder survives a restart (a
// second pgstore.Open over the same database).
func TestPgCreditLedgerRemainder(t *testing.T) {
	dsn := testDSN(t)
	pg, err := pgstore.Open(context.Background(), dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(pg.Close)
	truncateTables(t, dsn, "credit_ledger", "drawdown_remainders")
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

	// The combined write lands both the entry and the remainder.
	e := billing.LedgerEntry{OrgID: "o1", Kind: billing.KindUsageDrawdown, Amount: -1, Key: "w1", At: now, Note: "usage drawdown"}
	if err := l.AppendWithRemainder(ctx, e, 231); err != nil {
		t.Fatalf("append with remainder: %v", err)
	}
	rem, _ = l.Remainder(ctx, "o1")
	if rem != 231 {
		t.Fatalf("remainder = %d, want 231", rem)
	}
	bal, _ := l.Balance(ctx, "o1")
	if bal != -1 {
		t.Fatalf("balance = %d, want -1 (the appended debit)", int64(bal))
	}

	// A duplicate key is atomic: ErrDuplicateEntry, and NEITHER the entries nor
	// the remainder move (a replayed drawdown cannot double-count the carry).
	if err := l.AppendWithRemainder(ctx, e, 999); !errors.Is(err, billing.ErrDuplicateEntry) {
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
	e2 := billing.LedgerEntry{OrgID: "o1", Kind: billing.KindUsageDrawdown, Amount: -1, Key: "w2", At: now, Note: "usage drawdown"}
	if err := l.AppendWithRemainder(ctx, e2, -461); err != nil {
		t.Fatalf("append negative remainder: %v", err)
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
