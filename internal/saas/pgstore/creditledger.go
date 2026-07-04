package pgstore

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"mitos.run/mitos/internal/saas/billing"
)

// PgCreditLedger is the durable, append-only credit ledger. A balance is the
// signed sum of an org's entries; a non-empty idempotency key is unique per org.
type PgCreditLedger struct {
	pool *pgxpool.Pool
}

// NewPgCreditLedger returns a PgCreditLedger backed by pool.
func NewPgCreditLedger(pool *pgxpool.Pool) *PgCreditLedger { return &PgCreditLedger{pool: pool} }

// compile-time assertion that PgCreditLedger satisfies the CreditLedger contract.
var _ billing.CreditLedger = (*PgCreditLedger)(nil)

// Append inserts one entry. If the entry has a non-empty Key that already
// exists for the org, it returns billing.ErrDuplicateEntry and changes nothing.
func (l *PgCreditLedger) Append(ctx context.Context, e billing.LedgerEntry) error {
	const q = `
        INSERT INTO credit_ledger (org_id, kind, amount, idem_key, at, note)
        VALUES ($1, $2, $3, $4, $5, $6)`
	_, err := l.pool.Exec(ctx, q, e.OrgID, string(e.Kind), int64(e.Amount), e.Key, e.At, e.Note)
	if e.Key != "" && isUniqueViolation(err) {
		return billing.ErrDuplicateEntry
	}
	if err != nil {
		return fmt.Errorf("append ledger entry: %w", err)
	}
	return nil
}

// Remainder returns the org's carried drawdown remainder in milli-cents
// (migration 0010, issue #662). An org with no row reads as zero, exactly like
// the in-memory ledger.
func (l *PgCreditLedger) Remainder(ctx context.Context, orgID string) (int64, error) {
	const q = `SELECT milli_cents FROM drawdown_remainders WHERE org_id = $1`
	var m int64
	err := l.pool.QueryRow(ctx, q, orgID).Scan(&m)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read drawdown remainder: %w", err)
	}
	return m, nil
}

// SettleWindow commits one drawdown settle in ONE transaction (issue #672,
// extending the issue #666 AppendWithRemainder path): the processed-window
// marker, the org's drawdown remainder, and, only when the amount is nonzero,
// the ledger entry. A duplicate marker (the replayed-window case) or a
// duplicate non-empty entry key (a window settled before the marker table
// existed) rolls the whole transaction back and returns
// billing.ErrDuplicateEntry with everything untouched, which is what makes a
// replayed drawdown window unable to double-debit or double-count the carry.
func (l *PgCreditLedger) SettleWindow(ctx context.Context, e billing.LedgerEntry, remainderMilliCents int64, w billing.ProcessedWindow) error {
	tx, err := l.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin drawdown settle: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	const mark = `
        INSERT INTO processed_usage_windows (org_id, sandbox_id, window_at)
        VALUES ($1, $2, $3)`
	_, err = tx.Exec(ctx, mark, w.OrgID, w.SandboxID, w.Window)
	if isUniqueViolation(err) {
		return billing.ErrDuplicateEntry
	}
	if err != nil {
		return fmt.Errorf("mark processed window: %w", err)
	}

	if e.Amount != 0 {
		const insert = `
        INSERT INTO credit_ledger (org_id, kind, amount, idem_key, at, note)
        VALUES ($1, $2, $3, $4, $5, $6)`
		_, err = tx.Exec(ctx, insert, e.OrgID, string(e.Kind), int64(e.Amount), e.Key, e.At, e.Note)
		if e.Key != "" && isUniqueViolation(err) {
			return billing.ErrDuplicateEntry
		}
		if err != nil {
			return fmt.Errorf("append ledger entry: %w", err)
		}
	}

	const upsert = `
        INSERT INTO drawdown_remainders (org_id, milli_cents, updated_at)
        VALUES ($1, $2, now())
        ON CONFLICT (org_id) DO UPDATE SET
            milli_cents = EXCLUDED.milli_cents,
            updated_at  = now()`
	if _, err := tx.Exec(ctx, upsert, e.OrgID, remainderMilliCents); err != nil {
		return fmt.Errorf("set drawdown remainder: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit drawdown settle: %w", err)
	}
	return nil
}

// SettledWindowKeys returns the org's already-settled drawdown keys since the
// given instant: the processed-window markers (by window time) unioned with
// the keyed usage_drawdown ledger rows (by settle time). The ledger half keeps
// windows settled BEFORE migration 0011 deduplicated across the deploy (their
// only trace is the ledger row) and is removable once one drawdown lookback
// horizon has passed since that deploy. A settle always happens after its
// window closes, so at >= since never hides a row whose window is in scope.
func (l *PgCreditLedger) SettledWindowKeys(ctx context.Context, orgID string, since time.Time) (map[string]bool, error) {
	out := map[string]bool{}

	const markers = `
        SELECT sandbox_id, window_at FROM processed_usage_windows
        WHERE org_id = $1 AND window_at >= $2`
	rows, err := l.pool.Query(ctx, markers, orgID, since)
	if err != nil {
		return nil, fmt.Errorf("read processed windows: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var sandboxID string
		var window time.Time
		if err := rows.Scan(&sandboxID, &window); err != nil {
			return nil, fmt.Errorf("scan processed window: %w", err)
		}
		out[billing.DrawdownKey(orgID, sandboxID, window)] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate processed windows: %w", err)
	}

	const legacy = `
        SELECT idem_key FROM credit_ledger
        WHERE org_id = $1 AND kind = $2 AND idem_key <> '' AND at >= $3`
	lrows, err := l.pool.Query(ctx, legacy, orgID, string(billing.KindUsageDrawdown), since)
	if err != nil {
		return nil, fmt.Errorf("read settled ledger keys: %w", err)
	}
	defer lrows.Close()
	for lrows.Next() {
		var key string
		if err := lrows.Scan(&key); err != nil {
			return nil, fmt.Errorf("scan settled ledger key: %w", err)
		}
		out[key] = true
	}
	if err := lrows.Err(); err != nil {
		return nil, fmt.Errorf("iterate settled ledger keys: %w", err)
	}
	return out, nil
}

// PruneProcessedWindows deletes markers whose window is before olderThan and
// returns how many were removed.
func (l *PgCreditLedger) PruneProcessedWindows(ctx context.Context, olderThan time.Time) (int64, error) {
	tag, err := l.pool.Exec(ctx, `DELETE FROM processed_usage_windows WHERE window_at < $1`, olderThan)
	if err != nil {
		return 0, fmt.Errorf("prune processed windows: %w", err)
	}
	return tag.RowsAffected(), nil
}

// Balance returns the signed sum of the org's ledger entries. An org with no
// entries has a balance of zero.
func (l *PgCreditLedger) Balance(ctx context.Context, orgID string) (billing.Money, error) {
	const q = `SELECT COALESCE(SUM(amount), 0) FROM credit_ledger WHERE org_id = $1`
	var sum int64
	if err := l.pool.QueryRow(ctx, q, orgID).Scan(&sum); err != nil {
		return 0, fmt.Errorf("ledger balance: %w", err)
	}
	return billing.Money(sum), nil
}

// Entries returns the org's entries in append order (ascending by primary key).
func (l *PgCreditLedger) Entries(ctx context.Context, orgID string) ([]billing.LedgerEntry, error) {
	const q = `SELECT org_id, kind, amount, idem_key, at, note FROM credit_ledger WHERE org_id = $1 ORDER BY id`
	rows, err := l.pool.Query(ctx, q, orgID)
	if err != nil {
		return nil, fmt.Errorf("ledger entries: %w", err)
	}
	defer rows.Close()
	var out []billing.LedgerEntry
	for rows.Next() {
		var e billing.LedgerEntry
		var kind string
		var amount int64
		if err := rows.Scan(&e.OrgID, &kind, &amount, &e.Key, &e.At, &e.Note); err != nil {
			return nil, fmt.Errorf("scan ledger entry: %w", err)
		}
		e.Kind = billing.EntryKind(kind)
		e.Amount = billing.Money(amount)
		e.At = e.At.UTC()
		out = append(out, e)
	}
	return out, rows.Err()
}

// EntriesSince implements billing.ScopedLedgerReader: the org's entries with
// at >= since, in append order, served from the (org_id, at) index (migration
// 0012) so the drawdown driver's per-cycle spend-cap read is bounded by the
// month, not the org's lifetime history.
func (l *PgCreditLedger) EntriesSince(ctx context.Context, orgID string, since time.Time) ([]billing.LedgerEntry, error) {
	const q = `SELECT org_id, kind, amount, idem_key, at, note FROM credit_ledger WHERE org_id = $1 AND at >= $2 ORDER BY id`
	rows, err := l.pool.Query(ctx, q, orgID, since)
	if err != nil {
		return nil, fmt.Errorf("ledger entries since: %w", err)
	}
	defer rows.Close()
	var out []billing.LedgerEntry
	for rows.Next() {
		var e billing.LedgerEntry
		var kind string
		var amount int64
		if err := rows.Scan(&e.OrgID, &kind, &amount, &e.Key, &e.At, &e.Note); err != nil {
			return nil, fmt.Errorf("scan ledger entry: %w", err)
		}
		e.Kind = billing.EntryKind(kind)
		e.Amount = billing.Money(amount)
		e.At = e.At.UTC()
		out = append(out, e)
	}
	return out, rows.Err()
}

// compile-time assertion: the Postgres ledger serves the scoped read.
var _ billing.ScopedLedgerReader = (*PgCreditLedger)(nil)
