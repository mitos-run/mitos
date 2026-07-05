package pgstore

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"mitos.run/mitos/internal/saas"
)

// invitationsPendingUniqueIndex is the partial unique index name from
// migrations/0016_invitations_pending_unique.sql: ON (org_id, lower(email))
// WHERE state = 'pending'. A violation of THIS specific index means
// saas.ErrInvitePending (an invitation is already pending for this
// address); a violation of any other unique constraint on the table (a
// duplicate id or token_hash) means a genuine saas.ErrConflict.
const invitationsPendingUniqueIndex = "invitations_org_email_pending_idx"

// isPendingInviteConflict reports whether err is specifically a violation of
// invitationsPendingUniqueIndex: an invitation is already pending for this
// (org_id, lower(email)). Unlike isUniqueViolation (pgstore.go), which
// matches on SQLSTATE 23505 alone, this ALSO requires the constraint name so
// it never misclassifies a duplicate id or token_hash (a genuine
// saas.ErrConflict) as a pending-invite conflict. Used only by
// CreateInvitation and ReplaceInvitation, which check this first and fall
// back to isUniqueViolation for every other unique violation on the table.
func isPendingInviteConflict(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == pgUniqueViolation && pgErr.ConstraintName == invitationsPendingUniqueIndex
}

// CreateInvitation inserts a new invitation row. A duplicate id or token hash
// violates a unique constraint and surfaces as saas.ErrConflict, matching
// MemStore. A duplicate still-pending (org_id, lower(email)) violates the
// partial unique index guarding InvitationService.CreateInvite's
// check-then-act race (migration 0016) and surfaces as saas.ErrInvitePending,
// also matching MemStore's atomic in-lock check.
//
// created_at and expires_at are NOT NULL columns (migrations/0014_invitations.sql),
// so inserting NULL for either would violate the constraint. A zero CreatedAt
// is defaulted to time.Now().UTC(), and a zero ExpiresAt to CreatedAt (after
// its own defaulting) plus saas.InvitationTTL: an invitation created without
// explicit times gets the standard 7-day lifetime, neither expired at birth
// (defaulting to now) nor immortal (MemStore's old zero-means-never-expires
// reading). MemStore.CreateInvitation applies the identical defaults; the
// contract subtest InvitationZeroTimesDefaulted pins both so they cannot
// diverge again. Real callers (internal/saas/invites.go) always set both
// fields explicitly, so this only matters for defensively-constructed
// invitations.
func (s *PgStore) CreateInvitation(ctx context.Context, inv saas.Invitation) error {
	if inv.CreatedAt.IsZero() {
		inv.CreatedAt = time.Now().UTC()
	}
	if inv.ExpiresAt.IsZero() {
		inv.ExpiresAt = inv.CreatedAt.Add(saas.InvitationTTL)
	}
	const q = `
        INSERT INTO invitations (id, org_id, email, role, token_hash, state, inviter_id, created_at, expires_at)
        VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`
	_, err := s.pool.Exec(ctx, q,
		inv.ID, inv.OrgID, inv.Email, string(inv.Role), inv.TokenHash, string(inv.State), inv.InviterID,
		timePtr(inv.CreatedAt), timePtr(inv.ExpiresAt))
	if isPendingInviteConflict(err) {
		return saas.ErrInvitePending
	}
	if isUniqueViolation(err) {
		return saas.ErrConflict
	}
	if err != nil {
		return fmt.Errorf("create invitation: %w", err)
	}
	return nil
}

// ReplaceInvitation atomically deletes the invitation identified by oldID and
// inserts fresh in its place, both inside one transaction. ResendInvite uses
// this instead of a separate Remove-then-Create pair so the replacement
// invitation never has to coexist, even momentarily, with the original row
// it supersedes: the partial unique index on (org_id, lower(email)) WHERE
// state = 'pending' (migration 0016) would otherwise reject the insert while
// the original is still pending. A failure rolls back the whole transaction,
// so oldID's row is left exactly as it was, never lost and never duplicated.
// Returns saas.ErrNotFound if oldID does not exist.
func (s *PgStore) ReplaceInvitation(ctx context.Context, oldID string, fresh saas.Invitation) error {
	if fresh.CreatedAt.IsZero() {
		fresh.CreatedAt = time.Now().UTC()
	}
	if fresh.ExpiresAt.IsZero() {
		fresh.ExpiresAt = fresh.CreatedAt.Add(saas.InvitationTTL)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin replace invitation: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	tag, err := tx.Exec(ctx, `DELETE FROM invitations WHERE id = $1`, oldID)
	if err != nil {
		return fmt.Errorf("replace invitation: delete old: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return saas.ErrNotFound
	}

	const insertQ = `
        INSERT INTO invitations (id, org_id, email, role, token_hash, state, inviter_id, created_at, expires_at)
        VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`
	_, err = tx.Exec(ctx, insertQ,
		fresh.ID, fresh.OrgID, fresh.Email, string(fresh.Role), fresh.TokenHash, string(fresh.State), fresh.InviterID,
		timePtr(fresh.CreatedAt), timePtr(fresh.ExpiresAt))
	if isPendingInviteConflict(err) {
		return saas.ErrInvitePending
	}
	if isUniqueViolation(err) {
		return saas.ErrConflict
	}
	if err != nil {
		return fmt.Errorf("replace invitation: insert fresh: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit replace invitation: %w", err)
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
