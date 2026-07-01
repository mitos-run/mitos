package pgstore

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"mitos.run/mitos/internal/saas/billing"
)

// PgSpendCapStore is the durable, per-org spend-cap store. One row per org;
// Set upserts so a second call for the same org updates, never errors on a
// duplicate key. Amounts are stored as integer cents (BIGINT), matching
// billing.Money. A zero value means "no cap", exactly as the in-memory store
// and the console reader already treat it.
type PgSpendCapStore struct {
	pool *pgxpool.Pool
}

// NewPgSpendCapStore returns a PgSpendCapStore backed by pool.
func NewPgSpendCapStore(pool *pgxpool.Pool) *PgSpendCapStore {
	return &PgSpendCapStore{pool: pool}
}

// compile-time assertion that PgSpendCapStore satisfies the SpendCapStore contract.
var _ billing.SpendCapStore = (*PgSpendCapStore)(nil)

// Set upserts the spend cap for the org identified by cap.OrgID. A second Set
// for the same org updates the existing row rather than returning an error.
func (s *PgSpendCapStore) Set(ctx context.Context, cap billing.SpendCap) error {
	const q = `
        INSERT INTO spend_caps (org_id, soft_cap, hard_cap)
        VALUES ($1, $2, $3)
        ON CONFLICT (org_id) DO UPDATE SET
            soft_cap = EXCLUDED.soft_cap,
            hard_cap = EXCLUDED.hard_cap`
	_, err := s.pool.Exec(ctx, q, cap.OrgID, int64(cap.SoftCap), int64(cap.HardCap))
	if err != nil {
		return fmt.Errorf("set spend cap: %w", err)
	}
	return nil
}

// Get returns the org's spend cap. If no cap has been set for the org it
// returns (billing.SpendCap{}, false, nil). Any other error is wrapped and
// returned as-is.
func (s *PgSpendCapStore) Get(ctx context.Context, orgID string) (billing.SpendCap, bool, error) {
	const q = `SELECT soft_cap, hard_cap FROM spend_caps WHERE org_id = $1`
	var soft, hard int64
	err := s.pool.QueryRow(ctx, q, orgID).Scan(&soft, &hard)
	if errors.Is(err, pgx.ErrNoRows) {
		return billing.SpendCap{}, false, nil
	}
	if err != nil {
		return billing.SpendCap{}, false, fmt.Errorf("get spend cap: %w", err)
	}
	return billing.SpendCap{
		OrgID:   orgID,
		SoftCap: billing.Money(soft),
		HardCap: billing.Money(hard),
	}, true, nil
}
