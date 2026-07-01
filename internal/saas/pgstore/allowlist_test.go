package pgstore_test

import (
	"context"
	"testing"
	"time"

	"mitos.run/mitos/internal/saas/pgstore"
)

func TestPgAllowlist(t *testing.T) {
	dsn := testDSN(t)
	pg, err := pgstore.Open(context.Background(), dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(pg.Close)
	truncateTables(t, dsn, "allowlist")
	al := pgstore.NewPgAllowlist(pg.Pool(), []string{"mitos.run"})
	ctx := context.Background()
	now := time.Now()

	// Unset email returns false.
	ok, err := al.IsAllowed(ctx, "unknown@example.com")
	if err != nil {
		t.Fatalf("IsAllowed unset: %v", err)
	}
	if ok {
		t.Fatal("IsAllowed unset: want false, got true")
	}

	// Add then IsAllowed returns true.
	if err := al.Add(ctx, "alice@example.com", "test", now); err != nil {
		t.Fatalf("Add: %v", err)
	}
	ok, err = al.IsAllowed(ctx, "alice@example.com")
	if err != nil {
		t.Fatalf("IsAllowed after Add: %v", err)
	}
	if !ok {
		t.Fatal("IsAllowed after Add: want true, got false")
	}

	// Add twice for the same email is idempotent (no duplicate-key error).
	if err := al.Add(ctx, "alice@example.com", "note2", now); err != nil {
		t.Fatalf("Add idempotent: %v", err)
	}

	// Auto-allow domain is true without any row.
	ok, err = al.IsAllowed(ctx, "anyone@mitos.run")
	if err != nil {
		t.Fatalf("IsAllowed auto-allow domain: %v", err)
	}
	if !ok {
		t.Fatal("IsAllowed auto-allow domain: want true, got false")
	}
}
