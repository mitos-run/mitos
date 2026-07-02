package pgstore

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"mitos.run/mitos/internal/saas/quota"
)

// PgSuspensionStore is the durable, replica-shared quota.SuspensionStore (issue
// #615). It makes the abuse/billing kill-switch authoritative across gateway
// restarts and across every gateway replica: a suspension written by one replica
// (or by the console/billing path) is read by all of them from the same table.
// The in-memory MemSuspensionStore remains the dev fallback behind the same
// interface.
//
// Semantics match MemSuspensionStore exactly (the behavioral reference):
//   - Suspend is idempotent: re-suspending updates reason, note, and manual_hold
//     but keeps the FIRST suspension time.
//   - Lift deletes the row and reports whether the org had been suspended.
//   - IsSuspended returns the record for a suspended org.
//
// Reads sit on the gateway's hot request path; wrap this store in
// quota.NewCachedSuspensionStore so the gateway does not query Postgres per
// request. Error posture: this store returns errors as-is and the enforcer
// treats a suspension-store error as a DENY (fail closed), so a Postgres outage
// never becomes an open door for a possibly-suspended org.
//
// No secret ever touches this table: rows carry the org id, a reason label, a
// non-secret note, and timestamps only.
type PgSuspensionStore struct {
	pool *pgxpool.Pool
}

// NewPgSuspensionStore returns a PgSuspensionStore backed by pool.
func NewPgSuspensionStore(pool *pgxpool.Pool) *PgSuspensionStore {
	return &PgSuspensionStore{pool: pool}
}

// compile-time assertion that PgSuspensionStore satisfies the contract.
var _ quota.SuspensionStore = (*PgSuspensionStore)(nil)

// Suspend marks the org suspended. It is idempotent: re-suspending updates the
// reason, note, and manual hold but keeps the first suspension time (the
// ON CONFLICT branch deliberately does NOT touch suspended_at), matching
// MemSuspensionStore.
func (s *PgSuspensionStore) Suspend(ctx context.Context, sus quota.Suspension) error {
	const q = `
        INSERT INTO suspensions (org_id, reason, note, suspended_at, manual_hold)
        VALUES ($1, $2, $3, $4, $5)
        ON CONFLICT (org_id) DO UPDATE SET
            reason      = EXCLUDED.reason,
            note        = EXCLUDED.note,
            manual_hold = EXCLUDED.manual_hold`
	if _, err := s.pool.Exec(ctx, q, sus.OrgID, string(sus.Reason), sus.Note, sus.At, sus.ManualHold); err != nil {
		return fmt.Errorf("suspend org: %w", err)
	}
	return nil
}

// Lift clears an org's suspension by deleting its row. It returns whether the
// org had been suspended (RowsAffected == 1).
func (s *PgSuspensionStore) Lift(ctx context.Context, orgID string) (bool, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM suspensions WHERE org_id = $1`, orgID)
	if err != nil {
		return false, fmt.Errorf("lift suspension: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// IsSuspended reports whether the org is currently suspended and, if so, the
// record. A missing row is (zero, false, nil), never an error.
func (s *PgSuspensionStore) IsSuspended(ctx context.Context, orgID string) (quota.Suspension, bool, error) {
	const q = `SELECT reason, note, suspended_at, manual_hold FROM suspensions WHERE org_id = $1`
	sus := quota.Suspension{OrgID: orgID}
	var reason string
	err := s.pool.QueryRow(ctx, q, orgID).Scan(&reason, &sus.Note, &sus.At, &sus.ManualHold)
	if errors.Is(err, pgx.ErrNoRows) {
		return quota.Suspension{}, false, nil
	}
	if err != nil {
		return quota.Suspension{}, false, fmt.Errorf("read suspension: %w", err)
	}
	sus.Reason = quota.SuspensionReason(reason)
	sus.At = sus.At.UTC()
	return sus, true, nil
}
