package pgstore

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"sort"

	"github.com/jackc/pgx/v5/pgxpool"
)

// migrationsFS holds the embedded schema migrations. Each file is applied once,
// in lexical (filename) order, and recorded in schema_migrations. A small
// embedded runner is deliberate: no heavy migration library, no external tool,
// just plain SQL applied inside a transaction and recorded so startup is
// idempotent.
//
//go:embed migrations/*.sql
var migrationsFS embed.FS

// migrate applies every unapplied migration in lexical order. It is safe to run
// on every startup: already-applied migrations are skipped, and each migration
// plus its bookkeeping row commit together in one transaction so a crash never
// leaves a half-applied migration recorded as done. A failure aborts startup
// with the offending filename (never any DSN or secret material).
func migrate(ctx context.Context, pool *pgxpool.Pool) error {
	if err := ensureMigrationsTable(ctx, pool); err != nil {
		return err
	}

	applied, err := appliedMigrations(ctx, pool)
	if err != nil {
		return err
	}

	names, err := migrationNames()
	if err != nil {
		return err
	}

	for _, name := range names {
		if applied[name] {
			continue
		}
		body, err := migrationsFS.ReadFile("migrations/" + name)
		if err != nil {
			return fmt.Errorf("read migration %s: %w", name, err)
		}
		if err := applyMigration(ctx, pool, name, string(body)); err != nil {
			return fmt.Errorf("apply migration %s: %w", name, err)
		}
	}
	return nil
}

// ensureMigrationsTable creates the bookkeeping table if it does not exist. It is
// itself idempotent (IF NOT EXISTS) so it can run on every startup.
func ensureMigrationsTable(ctx context.Context, pool *pgxpool.Pool) error {
	const ddl = `CREATE TABLE IF NOT EXISTS schema_migrations (
        version    TEXT        PRIMARY KEY,
        applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
    )`
	if _, err := pool.Exec(ctx, ddl); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}
	return nil
}

// appliedMigrations returns the set of versions already recorded.
func appliedMigrations(ctx context.Context, pool *pgxpool.Pool) (map[string]bool, error) {
	rows, err := pool.Query(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return nil, fmt.Errorf("read schema_migrations: %w", err)
	}
	defer rows.Close()

	applied := map[string]bool{}
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, fmt.Errorf("scan schema_migrations: %w", err)
		}
		applied[v] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate schema_migrations: %w", err)
	}
	return applied, nil
}

// migrationNames returns the embedded migration filenames in lexical order.
func migrationNames() ([]string, error) {
	entries, err := fs.ReadDir(migrationsFS, "migrations")
	if err != nil {
		return nil, fmt.Errorf("read migrations dir: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names, nil
}

// applyMigration runs one migration and records it in the same transaction. If
// the SQL or the bookkeeping insert fails, the whole transaction rolls back so
// the migration is never recorded as applied without its schema change landing.
func applyMigration(ctx context.Context, pool *pgxpool.Pool, name, body string) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, body); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations (version) VALUES ($1)`, name); err != nil {
		return fmt.Errorf("record migration: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}
