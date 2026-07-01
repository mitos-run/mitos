package pgstore

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"mitos.run/mitos/internal/saas/onboarding"
)

// PgPendingStore is the durable onboarding pending-signup and waitlist store.
// It implements onboarding.PendingStore over Postgres, mirroring the behavior
// of MemPendingStore: unknown token hashes return ErrPendingNotFound, and
// MarkVerified is idempotent when called with the same hash and account id.
type PgPendingStore struct {
	pool *pgxpool.Pool
}

// NewPgPendingStore returns a PgPendingStore backed by pool. The caller retains
// ownership of pool; Close on the parent PgStore releases it.
func NewPgPendingStore(pool *pgxpool.Pool) *PgPendingStore { return &PgPendingStore{pool: pool} }

// compile-time assertion that PgPendingStore satisfies the PendingStore contract.
var _ onboarding.PendingStore = (*PgPendingStore)(nil)

// PutPending inserts or updates a pending signup keyed by id. On a retry the
// row is fully replaced so a re-issued token (new hash) replaces the old one.
func (s *PgPendingStore) PutPending(ctx context.Context, p onboarding.PendingSignup) error {
	const q = `
        INSERT INTO pending_signups (id, email, token_hash, created_at, expires_at, verified, account_id, use_case)
        VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
        ON CONFLICT (id) DO UPDATE SET
            email      = EXCLUDED.email,
            token_hash = EXCLUDED.token_hash,
            created_at = EXCLUDED.created_at,
            expires_at = EXCLUDED.expires_at,
            verified   = EXCLUDED.verified,
            account_id = EXCLUDED.account_id,
            use_case   = EXCLUDED.use_case`
	_, err := s.pool.Exec(ctx, q,
		p.ID, p.Email, p.TokenHash, p.CreatedAt, p.ExpiresAt, p.Verified, p.AccountID, p.UseCase)
	if err != nil {
		return fmt.Errorf("put pending signup: %w", err)
	}
	return nil
}

// GetPendingByTokenHash returns the pending signup whose token_hash matches, or
// onboarding.ErrPendingNotFound when no row exists (matching MemPendingStore).
func (s *PgPendingStore) GetPendingByTokenHash(ctx context.Context, tokenHash string) (onboarding.PendingSignup, error) {
	const q = `
        SELECT id, email, token_hash, created_at, expires_at, verified, account_id, use_case
        FROM pending_signups
        WHERE token_hash = $1`
	var p onboarding.PendingSignup
	err := s.pool.QueryRow(ctx, q, tokenHash).Scan(
		&p.ID, &p.Email, &p.TokenHash, &p.CreatedAt, &p.ExpiresAt, &p.Verified, &p.AccountID, &p.UseCase)
	if errors.Is(err, pgx.ErrNoRows) {
		return onboarding.PendingSignup{}, onboarding.ErrPendingNotFound
	}
	if err != nil {
		return onboarding.PendingSignup{}, fmt.Errorf("get pending signup: %w", err)
	}
	p.CreatedAt = p.CreatedAt.UTC()
	p.ExpiresAt = p.ExpiresAt.UTC()
	return p, nil
}

// MarkVerified records that a pending signup was provisioned, storing the
// account id. Returns ErrPendingNotFound if no row matches the hash.
func (s *PgPendingStore) MarkVerified(ctx context.Context, tokenHash, accountID string) error {
	const q = `UPDATE pending_signups SET verified = TRUE, account_id = $2 WHERE token_hash = $1`
	tag, err := s.pool.Exec(ctx, q, tokenHash, accountID)
	if err != nil {
		return fmt.Errorf("mark verified: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return onboarding.ErrPendingNotFound
	}
	return nil
}

// AddWaitlist appends a waitlist entry.
func (s *PgPendingStore) AddWaitlist(ctx context.Context, e onboarding.WaitlistEntry) error {
	const q = `INSERT INTO waitlist_entries (email, created_at) VALUES ($1, $2)`
	if _, err := s.pool.Exec(ctx, q, e.Email, e.CreatedAt); err != nil {
		return fmt.Errorf("add waitlist: %w", err)
	}
	return nil
}

// Waitlist returns all waitlist entries in append order (by serial id).
func (s *PgPendingStore) Waitlist(ctx context.Context) ([]onboarding.WaitlistEntry, error) {
	const q = `SELECT email, created_at FROM waitlist_entries ORDER BY id`
	rows, err := s.pool.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("list waitlist: %w", err)
	}
	defer rows.Close()
	var out []onboarding.WaitlistEntry
	for rows.Next() {
		var e onboarding.WaitlistEntry
		if err := rows.Scan(&e.Email, &e.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan waitlist: %w", err)
		}
		e.CreatedAt = e.CreatedAt.UTC()
		out = append(out, e)
	}
	return out, rows.Err()
}
