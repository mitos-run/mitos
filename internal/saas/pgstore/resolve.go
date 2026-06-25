package pgstore

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"mitos.run/mitos/internal/saas"
)

// EnvDSN is the environment variable the binaries read for the Postgres DSN when
// the --database-dsn flag is empty. The chart injects it from a Secret.
const EnvDSN = "MITOS_DATABASE_DSN"

// ResolveStore selects the persistence backend for a binary. The dsn argument is
// the value of the --database-dsn flag; when empty, the MITOS_DATABASE_DSN
// environment variable is consulted. When a DSN is configured, it opens a durable
// PgStore (which runs migrations) and returns it plus a no-arg close func; on
// failure it returns the error so the caller can fail fast. When no DSN is set,
// it returns an in-memory MemStore with a no-op close and logs that persistence
// is in-memory (dev only).
//
// The DSN is a secret: this function logs only "database configured" /
// "in-memory" and the chosen backend, never the DSN value.
func ResolveStore(ctx context.Context, dsn string, logger *slog.Logger) (saas.Store, func(), error) {
	if dsn == "" {
		dsn = os.Getenv(EnvDSN)
	}
	if dsn == "" {
		logger.Warn("no database DSN configured; using in-memory persistence (DEV ONLY, all accounts/orgs/keys are lost on restart). Set --database-dsn or MITOS_DATABASE_DSN for durable Postgres persistence.")
		return saas.NewMemStore(), func() {}, nil
	}
	pg, err := Open(ctx, dsn)
	if err != nil {
		// Never include the DSN in the error.
		return nil, nil, fmt.Errorf("open durable postgres store: %w", err)
	}
	logger.Info("durable Postgres persistence configured", "backend", "postgres")
	return pg, pg.Close, nil
}
