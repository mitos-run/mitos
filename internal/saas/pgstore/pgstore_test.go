package pgstore_test

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"mitos.run/mitos/internal/saas"
	"mitos.run/mitos/internal/saas/pgstore"
	"mitos.run/mitos/internal/saas/storetest"
)

// testDSN returns the integration DSN or skips. Local `go test` without a
// database passes (the suite skips); CI sets MITOS_TEST_DATABASE_DSN against a
// postgres service so the pg run executes there.
func testDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("MITOS_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("MITOS_TEST_DATABASE_DSN unset; skipping Postgres integration tests (set it to run them, CI does)")
	}
	return dsn
}

// truncateAll clears every data table so each contract subtest starts from a
// clean slate, sharing one migrated database. schema_migrations is left intact.
func truncateAll(t *testing.T, dsn string) {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("open pool for truncate: %v", err)
	}
	defer pool.Close()
	_, err = pool.Exec(context.Background(),
		`TRUNCATE accounts, orgs, memberships, api_keys RESTART IDENTITY CASCADE`)
	if err != nil {
		t.Fatalf("truncate: %v", err)
	}
}

// TestPgStoreContract runs the shared saas.Store contract against PgStore. It
// skips without a DSN. Each subtest's factory truncates the tables first, so the
// subtests are isolated while reusing one migrated database.
func TestPgStoreContract(t *testing.T) {
	dsn := testDSN(t)
	storetest.RunContract(t, func(t *testing.T) saas.Store {
		t.Helper()
		truncateAll(t, dsn)
		s, err := pgstore.Open(context.Background(), dsn)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		t.Cleanup(s.Close)
		var _ saas.Store = s
		return s
	})
}

// TestOpenRunsMigrationsAndIsIdempotent asserts Open creates the schema on a
// fresh database and that opening a second time (migrations already applied) is a
// no-op that still yields a working store.
func TestOpenRunsMigrationsAndIsIdempotent(t *testing.T) {
	dsn := testDSN(t)
	ctx := context.Background()

	s1, err := pgstore.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	s1.Close()

	// Second Open: migrations are already recorded; this must not error or
	// re-apply (the runner skips applied versions).
	s2, err := pgstore.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("second Open (idempotent): %v", err)
	}
	defer s2.Close()

	truncateAll(t, dsn)
	// The store works after a second Open: a basic put/get round trip.
	if err := s2.PutOrg(ctx, saas.Organization{ID: "org-x", Name: "X"}); err != nil {
		t.Fatalf("PutOrg after second Open: %v", err)
	}
	if _, err := s2.GetOrg(ctx, "org-x"); err != nil {
		t.Fatalf("GetOrg after second Open: %v", err)
	}
}

// TestDSNNeverInError asserts a connection failure error never echoes the DSN. A
// leaked DSN in a log or error would expose the database password, so this is a
// hard secret-hygiene guarantee.
func TestDSNNeverInError(t *testing.T) {
	// A syntactically valid DSN pointing at a port nothing listens on, with a
	// recognizable secret in it. Open must fail, and the error must not contain
	// the password or the host:port secret material.
	const secret = "sup3rs3cr3tpassword"
	dsn := "postgres://mitos:" + secret + "@127.0.0.1:1/mitos_nope?sslmode=disable&connect_timeout=1"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	_, err := pgstore.Open(ctx, dsn)
	if err == nil {
		t.Skip("Open unexpectedly succeeded against an unreachable DSN; cannot assert error hygiene")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("error leaked the DSN password: %q", err.Error())
	}
	if strings.Contains(err.Error(), dsn) {
		t.Fatalf("error leaked the full DSN: %q", err.Error())
	}
}
