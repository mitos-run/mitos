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
