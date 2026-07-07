package pgstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"mitos.run/mitos/internal/saas"
)

// PgSessionStore is the durable browser-session store. It stores only the
// sha256 hash of the session token (matching internal/saas/session.go), never
// the raw token.
type PgSessionStore struct {
	pool   *pgxpool.Pool
	maxAge time.Duration // absolute session lifetime; 0 disables expiry
}

// PgSessionStoreOption configures a PgSessionStore at construction.
type PgSessionStoreOption func(*PgSessionStore)

// WithPgSessionMaxAge sets the absolute session lifetime enforced at Resolve. A
// non-positive value disables expiry (issue #733, item 2).
func WithPgSessionMaxAge(d time.Duration) PgSessionStoreOption {
	return func(s *PgSessionStore) { s.maxAge = d }
}

// NewPgSessionStore constructs a PgSessionStore backed by the given pool.
func NewPgSessionStore(pool *pgxpool.Pool, opts ...PgSessionStoreOption) *PgSessionStore {
	s := &PgSessionStore{pool: pool}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// compile-time assertion that PgSessionStore satisfies saas.Sessions.
var _ saas.Sessions = (*PgSessionStore)(nil)

// hashSessionToken hashes a raw session token for at-rest storage. This is the
// same transform as hashSession in internal/saas/session.go (sha256-hex); that
// function is unexported so we replicate it here. The two must stay in sync.
func hashSessionToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// IssueSession records that token authenticates accountID with the given label.
// The raw token is never stored; only its sha256-hex hash is persisted. Returns
// an opaque session id derived from the hash prefix (unique and stable per token).
func (s *PgSessionStore) IssueSession(accountID, token, label string) string {
	h := hashSessionToken(token)
	id := "sess-" + h[:16]
	const q = `
		INSERT INTO sessions (id, token_hash, account_id, created_at, label)
		VALUES ($1, $2, $3, now(), $4)
		ON CONFLICT (token_hash) DO NOTHING`
	// Best-effort: errors here surface on Resolve as ErrSessionInvalid.
	_, _ = s.pool.Exec(context.Background(), q, id, h, accountID, label)
	return id
}

// Issue records that token authenticates accountID. It is a backward-compatible
// wrapper around IssueSession with a default label so existing callers compile
// unchanged.
func (s *PgSessionStore) Issue(accountID, token string) {
	s.IssueSession(accountID, token, "session")
}

// Resolve returns the account id for a session token, or saas.ErrSessionInvalid.
// When an absolute max-age is configured, a session older than it is treated as
// invalid (issue #733, item 2): the age filter is applied in the query so an
// expired session is indistinguishable from an unknown one.
func (s *PgSessionStore) Resolve(token string) (string, error) {
	var (
		q    string
		args []any
	)
	if s.maxAge > 0 {
		q = `SELECT account_id FROM sessions WHERE token_hash = $1 AND created_at > now() - make_interval(secs => $2)`
		args = []any{hashSessionToken(token), s.maxAge.Seconds()}
	} else {
		q = `SELECT account_id FROM sessions WHERE token_hash = $1`
		args = []any{hashSessionToken(token)}
	}
	var acct string
	err := s.pool.QueryRow(context.Background(), q, args...).Scan(&acct)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", saas.ErrSessionInvalid
	}
	if err != nil {
		return "", fmt.Errorf("resolve session: %w", err)
	}
	return acct, nil
}

// ListByAccount returns all sessions for accountID, most-recent-first.
func (s *PgSessionStore) ListByAccount(accountID string) []saas.Session {
	const q = `SELECT id, account_id, created_at, label FROM sessions WHERE account_id = $1 ORDER BY created_at DESC, id DESC`
	rows, err := s.pool.Query(context.Background(), q, accountID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []saas.Session
	for rows.Next() {
		var ss saas.Session
		if err := rows.Scan(&ss.ID, &ss.AccountID, &ss.CreatedAt, &ss.Label); err != nil {
			return out
		}
		ss.CreatedAt = ss.CreatedAt.UTC()
		out = append(out, ss)
	}
	if rows.Err() != nil {
		return nil
	}
	return out
}

// Revoke removes the session identified by sessionID from accountID's session
// set. If the session does not exist or belongs to a different account,
// saas.ErrNotFound is returned, matching the in-memory SessionStore.Revoke.
func (s *PgSessionStore) Revoke(accountID, sessionID string) error {
	const q = `DELETE FROM sessions WHERE account_id = $1 AND id = $2`
	tag, err := s.pool.Exec(context.Background(), q, accountID, sessionID)
	if err != nil {
		return fmt.Errorf("revoke session: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return saas.ErrNotFound
	}
	return nil
}

// RevokeAll removes every session belonging to accountID.
func (s *PgSessionStore) RevokeAll(accountID string) {
	const q = `DELETE FROM sessions WHERE account_id = $1`
	_, _ = s.pool.Exec(context.Background(), q, accountID)
}
