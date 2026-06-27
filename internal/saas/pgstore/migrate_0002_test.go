package pgstore_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"mitos.run/mitos/internal/saas/pgstore"
)

// pgstoreOpen is a thin helper that opens a PgStore (and runs all embedded
// migrations) against the given DSN. It is used by migration-specific tests
// that need to trigger the migration runner without going through the full
// contract harness.
func pgstoreOpen(t *testing.T, dsn string) (*pgstore.PgStore, error) {
	t.Helper()
	return pgstore.Open(context.Background(), dsn)
}

// openMigrated opens a PgStore so the embedded migrations are applied, then
// registers Close as a test cleanup. The returned store is discarded; callers
// use a separate pool to inspect the schema.
func openMigrated(t *testing.T, dsn string) {
	t.Helper()
	s, err := pgstoreOpen(t, dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(s.Close)
}

func TestMigration0002TablesExist(t *testing.T) {
	dsn := testDSN(t)
	// Open runs all embedded migrations including 0002.
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()
	openMigrated(t, dsn) // helper below applies migrations via pgstore.Open

	for _, table := range []string{"sessions", "credit_ledger", "pending_signups", "waitlist_entries"} {
		var exists bool
		err := pool.QueryRow(context.Background(),
			`SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name = $1)`, table).Scan(&exists)
		if err != nil {
			t.Fatalf("check %s: %v", table, err)
		}
		if !exists {
			t.Fatalf("table %s missing after migration 0002", table)
		}
	}
}
