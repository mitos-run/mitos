package pgstore

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"mitos.run/mitos/internal/saas/billing"
)

// PgStatusStore is the durable per-org billing StatusStore (the dunning state).
// One row per org; SetStatus upserts because provider webhooks replay. An org
// with no row is StatusActive (a new org is in good standing), exactly as the
// in-memory store behaves, so the two are interchangeable at the read boundary.
// Durability is the point (issue #614): a past_due or suspended state set by a
// billing webhook must survive a console restart, or an org suspended for
// nonpayment silently reverts to active.
type PgStatusStore struct {
	pool *pgxpool.Pool
}

// NewPgStatusStore returns a PgStatusStore backed by pool.
func NewPgStatusStore(pool *pgxpool.Pool) *PgStatusStore {
	return &PgStatusStore{pool: pool}
}

// compile-time assertion that PgStatusStore satisfies the StatusStore contract.
var _ billing.StatusStore = (*PgStatusStore)(nil)

// Status returns the org's billing status, or StatusActive when none has been
// recorded.
func (s *PgStatusStore) Status(ctx context.Context, orgID string) (billing.BillingStatus, error) {
	const q = `SELECT status FROM billing_status WHERE org_id = $1`
	var st string
	err := s.pool.QueryRow(ctx, q, orgID).Scan(&st)
	if errors.Is(err, pgx.ErrNoRows) {
		return billing.StatusActive, nil
	}
	if err != nil {
		return "", fmt.Errorf("billing status: %w", err)
	}
	return billing.BillingStatus(st), nil
}

// SetStatus upserts the org's billing status. A second SetStatus for the same
// org updates the existing row (webhooks replay; the dunning machine
// transitions), never errors on a duplicate key.
func (s *PgStatusStore) SetStatus(ctx context.Context, orgID string, st billing.BillingStatus) error {
	const q = `
        INSERT INTO billing_status (org_id, status, updated_at)
        VALUES ($1, $2, now())
        ON CONFLICT (org_id) DO UPDATE SET
            status = EXCLUDED.status,
            updated_at = now()`
	if _, err := s.pool.Exec(ctx, q, orgID, string(st)); err != nil {
		return fmt.Errorf("set billing status: %w", err)
	}
	return nil
}
