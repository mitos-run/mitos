package pgstore_test

import (
	"context"
	"testing"

	"mitos.run/mitos/internal/saas/billing"
	"mitos.run/mitos/internal/saas/pgstore"
)

func TestPgStatusStore(t *testing.T) {
	dsn := testDSN(t)
	pg, err := pgstore.Open(context.Background(), dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(pg.Close)
	truncateTables(t, dsn, "billing_status")
	s := pgstore.NewPgStatusStore(pg.Pool())
	ctx := context.Background()

	// An org with no recorded status is active (a new org is in good standing),
	// exactly like the in-memory store.
	got, err := s.Status(ctx, "org-fresh")
	if err != nil {
		t.Fatalf("Status fresh: %v", err)
	}
	if got != billing.StatusActive {
		t.Fatalf("Status fresh = %q, want %q", got, billing.StatusActive)
	}

	// SetStatus then Status returns the recorded state.
	if err := s.SetStatus(ctx, "org-a", billing.StatusPastDue); err != nil {
		t.Fatalf("SetStatus: %v", err)
	}
	got, err = s.Status(ctx, "org-a")
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if got != billing.StatusPastDue {
		t.Fatalf("Status = %q, want %q", got, billing.StatusPastDue)
	}

	// SetStatus again for the SAME org UPDATES (upsert; webhooks replay and the
	// dunning machine transitions, neither may hit a duplicate-key error).
	if err := s.SetStatus(ctx, "org-a", billing.StatusSuspended); err != nil {
		t.Fatalf("SetStatus upsert: %v", err)
	}
	got, err = s.Status(ctx, "org-a")
	if err != nil {
		t.Fatalf("Status after upsert: %v", err)
	}
	if got != billing.StatusSuspended {
		t.Fatalf("Status after upsert = %q, want %q", got, billing.StatusSuspended)
	}

	// Restart survival: a brand-new store over a brand-new pool (the console
	// process restarting) still sees the suspension. This is the money-relevant
	// property from issue #614: a suspended org must not revert to active.
	pg2, err := pgstore.Open(context.Background(), dsn)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(pg2.Close)
	s2 := pgstore.NewPgStatusStore(pg2.Pool())
	got, err = s2.Status(ctx, "org-a")
	if err != nil {
		t.Fatalf("Status after restart: %v", err)
	}
	if got != billing.StatusSuspended {
		t.Fatalf("Status after restart = %q, want %q (suspension lost on restart)", got, billing.StatusSuspended)
	}
}
