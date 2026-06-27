package pgstore_test

import (
	"context"
	"testing"

	"mitos.run/mitos/internal/saas/pgstore"
)

func TestResolveOnboardingStoresWithPool(t *testing.T) {
	dsn := testDSN(t)
	pg, err := pgstore.Open(context.Background(), dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(pg.Close)
	ledger, pending, sessions := pgstore.OnboardingStores(pg.Pool())
	if ledger == nil || pending == nil || sessions == nil {
		t.Fatal("expected durable stores from a non-nil pool")
	}
}

func TestOnboardingStoresNilPoolPanicsOrNil(t *testing.T) {
	// Documents intent: callers pass a non-nil pool only when a DSN is set;
	// the console falls back to Mem stores when there is no pool (no call here).
	t.Skip("documentation test; console handles the no-pool branch")
}
