package pgstore_test

import (
	"context"
	"testing"

	"mitos.run/mitos/internal/saas/billing"
	"mitos.run/mitos/internal/saas/pgstore"
)

func TestPgSpendCapStore(t *testing.T) {
	dsn := testDSN(t)
	pg, err := pgstore.Open(context.Background(), dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(pg.Close)
	truncateTables(t, dsn, "spend_caps")
	s := pgstore.NewPgSpendCapStore(pg.Pool())
	ctx := context.Background()

	// Get on an unset org returns (zero, false, nil).
	cap, ok, err := s.Get(ctx, "org-missing")
	if err != nil {
		t.Fatalf("Get missing: %v", err)
	}
	if ok {
		t.Fatalf("Get missing: ok = true, want false")
	}
	if cap != (billing.SpendCap{}) {
		t.Fatalf("Get missing: cap = %+v, want zero", cap)
	}

	// Set a cap then Get returns it with ok = true.
	want := billing.SpendCap{OrgID: "org-a", SoftCap: billing.USD(10), HardCap: billing.USD(50)}
	if err := s.Set(ctx, want); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, ok, err := s.Get(ctx, "org-a")
	if err != nil {
		t.Fatalf("Get after Set: %v", err)
	}
	if !ok {
		t.Fatalf("Get after Set: ok = false, want true")
	}
	if got != want {
		t.Fatalf("Get after Set: got %+v, want %+v", got, want)
	}

	// Set again for the SAME org UPDATES (upsert, not a duplicate-key error).
	updated := billing.SpendCap{OrgID: "org-a", SoftCap: billing.USD(20), HardCap: billing.USD(100)}
	if err := s.Set(ctx, updated); err != nil {
		t.Fatalf("Set upsert: %v", err)
	}
	got2, ok2, err := s.Get(ctx, "org-a")
	if err != nil {
		t.Fatalf("Get after upsert: %v", err)
	}
	if !ok2 {
		t.Fatalf("Get after upsert: ok = false, want true")
	}
	if got2 != updated {
		t.Fatalf("Get after upsert: got %+v, want %+v", got2, updated)
	}
}
