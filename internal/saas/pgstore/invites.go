package pgstore

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"mitos.run/mitos/internal/saas"
)

// CreateInvitation inserts a new invitation row. A duplicate id or token hash
// violates a unique constraint and surfaces as saas.ErrConflict, matching
// MemStore.
func (s *PgStore) CreateInvitation(ctx context.Context, inv saas.Invitation) error {
	const q = `
        INSERT INTO invitations (id, org_id, email, role, token_hash, state, inviter_id, created_at, expires_at)
        VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`
	_, err := s.pool.Exec(ctx, q,
		inv.ID, inv.OrgID, inv.Email, string(inv.Role), inv.TokenHash, string(inv.State), inv.InviterID,
		timePtr(inv.CreatedAt), timePtr(inv.ExpiresAt))
	if isUniqueViolation(err) {
		return saas.ErrConflict
	}
	if err != nil {
		return fmt.Errorf("create invitation: %w", err)
	}
	return nil
}

// ListInvitations returns every invitation for orgID, most-recently-created
// first, matching MemStore's ordering.
func (s *PgStore) ListInvitations(ctx context.Context, orgID string) ([]saas.Invitation, error) {
	const q = `
        SELECT id, org_id, email, role, token_hash, state, inviter_id, created_at, expires_at
        FROM invitations WHERE org_id = $1 ORDER BY created_at DESC`
	rows, err := s.pool.Query(ctx, q, orgID)
	if err != nil {
		return nil, fmt.Errorf("list invitations: %w", err)
	}
	defer rows.Close()

	out := []saas.Invitation{}
	for rows.Next() {
		inv, err := scanInvitation(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, inv)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate invitations: %w", err)
	}
	return out, nil
}

// GetInvitationByTokenHash returns the invitation whose token_hash matches,
// or saas.ErrNotFound.
func (s *PgStore) GetInvitationByTokenHash(ctx context.Context, hash string) (saas.Invitation, error) {
	const q = `
        SELECT id, org_id, email, role, token_hash, state, inviter_id, created_at, expires_at
        FROM invitations WHERE token_hash = $1`
	return scanInvitation(s.pool.QueryRow(ctx, q, hash))
}

// scanInvitation reads one invitation row, mapping no-rows to saas.ErrNotFound.
func scanInvitation(row pgx.Row) (saas.Invitation, error) {
	var inv saas.Invitation
	var role, state string
	var createdAt, expiresAt *time.Time
	if err := row.Scan(&inv.ID, &inv.OrgID, &inv.Email, &role, &inv.TokenHash, &state, &inv.InviterID, &createdAt, &expiresAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return saas.Invitation{}, saas.ErrNotFound
		}
		return saas.Invitation{}, fmt.Errorf("scan invitation: %w", err)
	}
	inv.Role = saas.Role(role)
	inv.State = saas.InvitationState(state)
	inv.CreatedAt = timeVal(createdAt)
	inv.ExpiresAt = timeVal(expiresAt)
	return inv, nil
}

// UpdateInvitationState transitions the stored state of the invitation
// identified by id. Returns saas.ErrNotFound for an unknown id.
func (s *PgStore) UpdateInvitationState(ctx context.Context, id string, state saas.InvitationState) error {
	const q = `UPDATE invitations SET state = $2 WHERE id = $1`
	tag, err := s.pool.Exec(ctx, q, id, string(state))
	if err != nil {
		return fmt.Errorf("update invitation state: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return saas.ErrNotFound
	}
	return nil
}

// RemoveInvitation permanently deletes the invitation identified by id.
// Returns saas.ErrNotFound for an unknown id.
func (s *PgStore) RemoveInvitation(ctx context.Context, id string) error {
	const q = `DELETE FROM invitations WHERE id = $1`
	tag, err := s.pool.Exec(ctx, q, id)
	if err != nil {
		return fmt.Errorf("remove invitation: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return saas.ErrNotFound
	}
	return nil
}

// DeleteMembership removes accountID's membership in orgID inside a
// transaction, mirroring SetMembershipRole's FOR UPDATE sole-owner lock so
// two concurrent last-member removals cannot both pass the owner-count
// check and leave the org ownerless.
func (s *PgStore) DeleteMembership(ctx context.Context, orgID, accountID string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin delete membership: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var role string
	err = tx.QueryRow(ctx,
		`SELECT role FROM memberships WHERE org_id = $1 AND account_id = $2 FOR UPDATE`,
		orgID, accountID).Scan(&role)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return saas.ErrNotFound
		}
		return fmt.Errorf("lookup membership: %w", err)
	}

	if saas.Role(role) == saas.RoleOwner {
		rows, err := tx.Query(ctx,
			`SELECT account_id FROM memberships WHERE org_id = $1 AND role = $2 FOR UPDATE`,
			orgID, string(saas.RoleOwner))
		if err != nil {
			return fmt.Errorf("lock owners: %w", err)
		}
		count := 0
		for rows.Next() {
			count++
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return fmt.Errorf("iterate owners: %w", err)
		}
		if count <= 1 {
			return saas.ErrLastOwner
		}
	}

	if _, err := tx.Exec(ctx, `DELETE FROM memberships WHERE org_id = $1 AND account_id = $2`, orgID, accountID); err != nil {
		return fmt.Errorf("delete membership: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit delete membership: %w", err)
	}
	return nil
}
