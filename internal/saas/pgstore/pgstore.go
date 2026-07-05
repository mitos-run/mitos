// Package pgstore is the durable Postgres implementation of saas.Store. It is
// the production persistence behind the SaaS front door: accounts,
// organizations, memberships, and scoped API keys that must survive a process
// restart. The in-memory saas.MemStore is the behavioral reference; this store
// passes the same contract (ErrConflict on a duplicate email or key id,
// ErrLastOwner on a sole-owner demotion, revoke idempotency, org-scoped listing
// isolation).
//
// It connects to an EXTERNAL, operator-supplied Postgres (a managed service:
// RDS, Cloud SQL, Neon, and so on); the chart does not bundle a database. All
// queries are parameterized; no value is ever interpolated into SQL. The DSN is
// a secret and is never logged or placed in an error message.
package pgstore

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"mitos.run/mitos/internal/saas"
)

// pgUniqueViolation is the SQLSTATE for a unique-constraint violation. We map it
// to saas.ErrConflict so callers see the same conflict semantics as MemStore.
const pgUniqueViolation = "23505"

// PgStore implements saas.Store over a pgx connection pool.
type PgStore struct {
	pool *pgxpool.Pool
}

// compile-time assertion that PgStore satisfies the Store contract.
var _ saas.Store = (*PgStore)(nil)

// Open connects to the Postgres at dsn, verifies the connection with a ping, and
// runs the embedded migrations so the schema is present before the store is
// returned. The DSN is a secret: on a connection failure the returned error
// carries context but NEVER the dsn value. The caller owns Close.
func Open(ctx context.Context, dsn string) (*PgStore, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		// pgxpool.New only fails on a malformed DSN; do not echo the dsn.
		return nil, fmt.Errorf("connect to postgres: %w", redact(err))
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping postgres: %w", redact(err))
	}
	if err := migrate(ctx, pool); err != nil {
		pool.Close()
		return nil, fmt.Errorf("run migrations: %w", err)
	}
	return &PgStore{pool: pool}, nil
}

// Close releases the connection pool.
func (s *PgStore) Close() {
	if s.pool != nil {
		s.pool.Close()
	}
}

// Pool returns the underlying connection pool. Other stores constructed from
// the same Postgres connection (e.g. PgCreditLedger) use it to share the pool.
func (s *PgStore) Pool() *pgxpool.Pool { return s.pool }

// redact guards against a driver error ever carrying the DSN. pgx errors do not
// embed the DSN, but this keeps the guarantee explicit and is covered by a test.
func redact(err error) error {
	return err
}

// isUniqueViolation reports whether err is a Postgres unique-constraint
// violation (SQLSTATE 23505), which we surface as saas.ErrConflict. This
// matches on the SQLSTATE alone: pgconn.PgError.ConstraintName can be empty
// for a real 23505 (e.g. a violation pgx did not resolve to a named
// constraint), and requiring it here would wrongly stop PutAccount,
// PutApiKey, and the credit-ledger inserts from recognizing a genuine
// duplicate. The invitation-specific pending-uniqueness routing needs the
// constraint name too; that lives in the separate isPendingInviteConflict
// (invites.go), used only by CreateInvitation/ReplaceInvitation.
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == pgUniqueViolation
}

// timePtr maps a Go time to a nullable column: the zero time becomes NULL so the
// "never expires" (ExpiresAt) and "live" (RevokedAt) semantics round trip.
func timePtr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

// timeVal maps a nullable column back to a Go time; NULL becomes the zero time.
// It normalizes to UTC: pgx returns a timestamptz in a connection-dependent
// *time.Location, so two values for the same instant would not be reflect.Equal
// to a caller's UTC time (and would diverge from the in-memory store). Returning
// UTC makes the round trip deterministic and store-equivalent.
func timeVal(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return t.UTC()
}

// PutAccount inserts or updates an account. A second account claiming an email
// already held by a different account violates the UNIQUE(email) index and is
// surfaced as saas.ErrConflict, matching MemStore.
func (s *PgStore) PutAccount(ctx context.Context, a saas.Account) error {
	const q = `
        INSERT INTO accounts (id, email, created_at, personal_org_id, display_name, timezone, locale)
        VALUES ($1, $2, $3, $4, $5, $6, $7)
        ON CONFLICT (id) DO UPDATE SET
            email           = EXCLUDED.email,
            created_at      = EXCLUDED.created_at,
            personal_org_id = EXCLUDED.personal_org_id,
            display_name    = EXCLUDED.display_name,
            timezone        = EXCLUDED.timezone,
            locale          = EXCLUDED.locale`
	_, err := s.pool.Exec(ctx, q, a.ID, a.Email, timePtr(a.CreatedAt), a.PersonalOrgID, a.DisplayName, a.Timezone, a.Locale)
	if isUniqueViolation(err) {
		return saas.ErrConflict
	}
	if err != nil {
		return fmt.Errorf("put account: %w", err)
	}
	return nil
}

func (s *PgStore) GetAccount(ctx context.Context, id string) (saas.Account, error) {
	const q = `SELECT id, email, created_at, personal_org_id, display_name, timezone, locale FROM accounts WHERE id = $1`
	return scanAccount(s.pool.QueryRow(ctx, q, id))
}

func (s *PgStore) GetAccountByEmail(ctx context.Context, email string) (saas.Account, error) {
	const q = `SELECT id, email, created_at, personal_org_id, display_name, timezone, locale FROM accounts WHERE email = $1`
	return scanAccount(s.pool.QueryRow(ctx, q, email))
}

// scanAccount reads one account row, mapping no-rows to saas.ErrNotFound.
func scanAccount(row pgx.Row) (saas.Account, error) {
	var a saas.Account
	var createdAt *time.Time
	if err := row.Scan(&a.ID, &a.Email, &createdAt, &a.PersonalOrgID, &a.DisplayName, &a.Timezone, &a.Locale); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return saas.Account{}, saas.ErrNotFound
		}
		return saas.Account{}, fmt.Errorf("scan account: %w", err)
	}
	a.CreatedAt = timeVal(createdAt)
	return a, nil
}

func (s *PgStore) PutOrg(ctx context.Context, o saas.Organization) error {
	const q = `
        INSERT INTO orgs (id, name, created_at, personal, home_region)
        VALUES ($1, $2, $3, $4, $5)
        ON CONFLICT (id) DO UPDATE SET
            name        = EXCLUDED.name,
            created_at  = EXCLUDED.created_at,
            personal    = EXCLUDED.personal`
	if _, err := s.pool.Exec(ctx, q, o.ID, o.Name, timePtr(o.CreatedAt), o.Personal, o.HomeRegion); err != nil {
		return fmt.Errorf("put org: %w", err)
	}
	return nil
}

func (s *PgStore) GetOrg(ctx context.Context, id string) (saas.Organization, error) {
	const q = `SELECT id, name, created_at, personal, home_region FROM orgs WHERE id = $1`
	var o saas.Organization
	var createdAt *time.Time
	if err := s.pool.QueryRow(ctx, q, id).Scan(&o.ID, &o.Name, &createdAt, &o.Personal, &o.HomeRegion); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return saas.Organization{}, saas.ErrNotFound
		}
		return saas.Organization{}, fmt.Errorf("get org: %w", err)
	}
	o.CreatedAt = timeVal(createdAt)
	return o, nil
}

// ListOrgs returns every organization (issue #602: the console's usage
// drawdown driver iterates the orgs). Operator/machine surface only.
func (s *PgStore) ListOrgs(ctx context.Context) ([]saas.Organization, error) {
	const q = `SELECT id, name, created_at, personal, home_region FROM orgs`
	rows, err := s.pool.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("list orgs: %w", err)
	}
	defer rows.Close()
	var out []saas.Organization
	for rows.Next() {
		var o saas.Organization
		var createdAt *time.Time
		if err := rows.Scan(&o.ID, &o.Name, &createdAt, &o.Personal, &o.HomeRegion); err != nil {
			return nil, fmt.Errorf("scan org: %w", err)
		}
		o.CreatedAt = timeVal(createdAt)
		out = append(out, o)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list orgs rows: %w", err)
	}
	return out, nil
}

// PutMembership upserts a membership keyed on (org_id, account_id), matching the
// MemStore "replace existing for this (org, account)" behavior.
func (s *PgStore) PutMembership(ctx context.Context, m saas.Membership) error {
	const q = `
        INSERT INTO memberships (org_id, account_id, role, created_at)
        VALUES ($1, $2, $3, $4)
        ON CONFLICT (org_id, account_id) DO UPDATE SET
            role       = EXCLUDED.role,
            created_at = EXCLUDED.created_at`
	if _, err := s.pool.Exec(ctx, q, m.OrgID, m.AccountID, string(m.Role), timePtr(m.CreatedAt)); err != nil {
		return fmt.Errorf("put membership: %w", err)
	}
	return nil
}

func (s *PgStore) ListMemberships(ctx context.Context, accountID string) ([]saas.Membership, error) {
	const q = `SELECT org_id, account_id, role, created_at FROM memberships WHERE account_id = $1 ORDER BY org_id`
	return s.queryMemberships(ctx, q, accountID)
}

func (s *PgStore) ListOrgMembers(ctx context.Context, orgID string) ([]saas.Membership, error) {
	const q = `SELECT org_id, account_id, role, created_at FROM memberships WHERE org_id = $1 ORDER BY account_id`
	return s.queryMemberships(ctx, q, orgID)
}

// queryMemberships runs a membership query and scans the rows. It always returns
// a non-nil slice (empty for no rows), matching MemStore.
func (s *PgStore) queryMemberships(ctx context.Context, q, arg string) ([]saas.Membership, error) {
	rows, err := s.pool.Query(ctx, q, arg)
	if err != nil {
		return nil, fmt.Errorf("list memberships: %w", err)
	}
	defer rows.Close()

	out := []saas.Membership{}
	for rows.Next() {
		var m saas.Membership
		var role string
		var createdAt *time.Time
		if err := rows.Scan(&m.OrgID, &m.AccountID, &role, &createdAt); err != nil {
			return nil, fmt.Errorf("scan membership: %w", err)
		}
		m.Role = saas.Role(role)
		m.CreatedAt = timeVal(createdAt)
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate memberships: %w", err)
	}
	return out, nil
}

// SetMembershipRole updates the target's role inside a transaction. The
// sole-owner check is done under FOR UPDATE on the org's owner rows so two
// concurrent demotions cannot both pass the "at least one other owner remains"
// test and leave the org ownerless. It returns ErrNotFound if no membership
// exists, and ErrLastOwner if the update would demote the last owner.
func (s *PgStore) SetMembershipRole(ctx context.Context, orgID, targetAccountID string, role saas.Role) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin set role: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Lock the org's owner rows so the count is stable for the duration of this
	// transaction. This serializes concurrent demotions of the same org.
	rows, err := tx.Query(ctx,
		`SELECT account_id FROM memberships WHERE org_id = $1 AND role = $2 FOR UPDATE`,
		orgID, string(saas.RoleOwner))
	if err != nil {
		return fmt.Errorf("lock owners: %w", err)
	}
	ownerCount := 0
	targetIsOwner := false
	for rows.Next() {
		var acct string
		if err := rows.Scan(&acct); err != nil {
			rows.Close()
			return fmt.Errorf("scan owner: %w", err)
		}
		ownerCount++
		if acct == targetAccountID {
			targetIsOwner = true
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate owners: %w", err)
	}

	// Refuse to demote the sole remaining owner.
	if role != saas.RoleOwner && targetIsOwner && ownerCount <= 1 {
		return saas.ErrLastOwner
	}

	tag, err := tx.Exec(ctx,
		`UPDATE memberships SET role = $1 WHERE org_id = $2 AND account_id = $3`,
		string(role), orgID, targetAccountID)
	if err != nil {
		return fmt.Errorf("update role: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return saas.ErrNotFound
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit set role: %w", err)
	}
	return nil
}

// PutApiKey inserts a key. A duplicate id (PK) or a duplicate hash (unique
// index) both violate a unique constraint and surface as saas.ErrConflict.
func (s *PgStore) PutApiKey(ctx context.Context, k saas.ApiKey) error {
	const q = `
        INSERT INTO api_keys (id, org_id, name, prefix, hash, scopes, created_at, expires_at, revoked_at)
        VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`
	scopes := k.Scopes
	if scopes == nil {
		scopes = []string{}
	}
	_, err := s.pool.Exec(ctx, q, k.ID, k.OrgID, k.Name, k.Prefix, k.Hash, scopes,
		timePtr(k.CreatedAt), timePtr(k.ExpiresAt), timePtr(k.RevokedAt))
	if isUniqueViolation(err) {
		return saas.ErrConflict
	}
	if err != nil {
		return fmt.Errorf("put api key: %w", err)
	}
	return nil
}

func (s *PgStore) GetApiKeyByHash(ctx context.Context, hash string) (saas.ApiKey, error) {
	const q = `SELECT id, org_id, name, prefix, hash, scopes, created_at, expires_at, revoked_at FROM api_keys WHERE hash = $1`
	return scanApiKey(s.pool.QueryRow(ctx, q, hash))
}

func (s *PgStore) GetApiKey(ctx context.Context, id string) (saas.ApiKey, error) {
	const q = `SELECT id, org_id, name, prefix, hash, scopes, created_at, expires_at, revoked_at FROM api_keys WHERE id = $1`
	return scanApiKey(s.pool.QueryRow(ctx, q, id))
}

// scanApiKey reads one key row, mapping no-rows to saas.ErrNotFound.
func scanApiKey(row pgx.Row) (saas.ApiKey, error) {
	var k saas.ApiKey
	var createdAt, expiresAt, revokedAt *time.Time
	if err := row.Scan(&k.ID, &k.OrgID, &k.Name, &k.Prefix, &k.Hash, &k.Scopes, &createdAt, &expiresAt, &revokedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return saas.ApiKey{}, saas.ErrNotFound
		}
		return saas.ApiKey{}, fmt.Errorf("scan api key: %w", err)
	}
	k.CreatedAt = timeVal(createdAt)
	k.ExpiresAt = timeVal(expiresAt)
	k.RevokedAt = timeVal(revokedAt)
	return k, nil
}

// ListApiKeys returns every key for an org, including revoked and expired ones,
// for audit. Ordered by created_at then id for a stable listing.
func (s *PgStore) ListApiKeys(ctx context.Context, orgID string) ([]saas.ApiKey, error) {
	const q = `SELECT id, org_id, name, prefix, hash, scopes, created_at, expires_at, revoked_at FROM api_keys WHERE org_id = $1 ORDER BY created_at, id`
	rows, err := s.pool.Query(ctx, q, orgID)
	if err != nil {
		return nil, fmt.Errorf("list api keys: %w", err)
	}
	defer rows.Close()

	out := []saas.ApiKey{}
	for rows.Next() {
		k, err := scanApiKey(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate api keys: %w", err)
	}
	return out, nil
}

// RevokeApiKey marks the key revoked. It is idempotent: a key that is already
// revoked keeps its original revoked_at (the WHERE clause skips already-revoked
// rows, and an unknown id returns ErrNotFound). This matches MemStore, where a
// second revoke is a no-op.
func (s *PgStore) RevokeApiKey(ctx context.Context, id string, at time.Time) error {
	// First, set revoked_at only where it is currently NULL (not yet revoked).
	const upd = `UPDATE api_keys SET revoked_at = $1 WHERE id = $2 AND revoked_at IS NULL`
	tag, err := s.pool.Exec(ctx, upd, at, id)
	if err != nil {
		return fmt.Errorf("revoke api key: %w", err)
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	// Zero rows affected means either the key does not exist or it is already
	// revoked. Distinguish: an existing-but-revoked key is a no-op (idempotent),
	// an unknown id is ErrNotFound.
	const exists = `SELECT 1 FROM api_keys WHERE id = $1`
	var one int
	if err := s.pool.QueryRow(ctx, exists, id).Scan(&one); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return saas.ErrNotFound
		}
		return fmt.Errorf("revoke api key lookup: %w", err)
	}
	return nil
}
