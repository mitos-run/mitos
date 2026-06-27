package pgstore

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"

	"mitos.run/mitos/internal/saas"
)

// EnvDSN is the environment variable the binaries read for the Postgres DSN when
// the --database-dsn flag is empty. The chart injects it from a Secret.
const EnvDSN = "MITOS_DATABASE_DSN"

// ResolveStoreWithPool selects the persistence backend for a binary and returns
// the underlying pool so callers can construct additional durable stores (credit
// ledger, pending signups, sessions) that share the same connection pool. The dsn
// argument is the value of the --database-dsn flag; when empty, the
// MITOS_DATABASE_DSN environment variable is consulted. When a DSN is configured,
// it opens a durable PgStore (which runs migrations) and returns it together with
// the non-nil pool and a close func. When no DSN is set, it returns an in-memory
// MemStore with a nil pool and a no-op close.
//
// The DSN is a secret: this function logs only "database configured" /
// "in-memory" and the chosen backend, never the DSN value.
func ResolveStoreWithPool(ctx context.Context, dsn string, logger *slog.Logger) (saas.Store, *pgxpool.Pool, func(), error) {
	if dsn == "" {
		dsn = os.Getenv(EnvDSN)
	}
	if dsn == "" {
		logger.Warn("no database DSN configured; using in-memory persistence (DEV ONLY, all accounts/orgs/keys are lost on restart). Set --database-dsn or MITOS_DATABASE_DSN for durable Postgres persistence.")
		return saas.NewMemStore(), nil, func() {}, nil
	}
	pg, err := Open(ctx, dsn)
	if err != nil {
		// Never include the DSN in the error.
		return nil, nil, nil, fmt.Errorf("open durable postgres store: %w", err)
	}
	logger.Info("durable Postgres persistence configured", "backend", "postgres")
	return pg, pg.Pool(), pg.Close, nil
}

// ResolveStore selects the persistence backend for a binary. It is a thin wrapper
// around ResolveStoreWithPool that drops the pool, kept for back-compat. Callers
// that need the pool to construct additional durable stores should use
// ResolveStoreWithPool instead.
func ResolveStore(ctx context.Context, dsn string, logger *slog.Logger) (saas.Store, func(), error) {
	store, _, closeFn, err := ResolveStoreWithPool(ctx, dsn, logger)
	return store, closeFn, err
}
