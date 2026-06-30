package pgstore_test

import (
	"context"
	"testing"

	"mitos.run/mitos/internal/saas/pgstore"
	"mitos.run/mitos/internal/usage"
	"mitos.run/mitos/internal/usage/usagestoretest"
)

// TestPgUsageStoreContract runs the shared UsageStore contract against the
// durable Postgres implementation, proving it behaves identically to the
// in-memory reference (idempotent upsert, per-org isolation, period bounds,
// per-org totals). It skips without MITOS_TEST_DATABASE_DSN; CI sets it against a
// Postgres service so the durable store is exercised there.
func TestPgUsageStoreContract(t *testing.T) {
	dsn := testDSN(t)
	usagestoretest.RunContract(t, func(t *testing.T) usage.UsageStore {
		t.Helper()
		// Open FIRST so the migrations create the schema (idempotent on every
		// subsequent call), THEN truncate for a clean slate.
		pg, err := pgstore.Open(context.Background(), dsn)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		truncateTables(t, dsn, "usage_records")
		t.Cleanup(pg.Close)
		return pgstore.NewPgUsageStore(pg.Pool())
	})
}
