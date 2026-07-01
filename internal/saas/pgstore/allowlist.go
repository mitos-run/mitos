package pgstore

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"mitos.run/mitos/internal/saas/onboarding"
)

// PgAllowlist is the durable Postgres implementation of onboarding.Allowlist.
// The auto-allow domain check happens in Go (not SQL) so the precedence logic
// mirrors MemAllowlist exactly; only the exact-row lookup hits the database.
type PgAllowlist struct {
	pool          *pgxpool.Pool
	autoAllowDoms map[string]struct{}
}

// NewPgAllowlist returns a PgAllowlist backed by pool. autoAllowDomains must
// be already lowercased with no leading '@'.
func NewPgAllowlist(pool *pgxpool.Pool, autoAllowDomains []string) *PgAllowlist {
	doms := make(map[string]struct{}, len(autoAllowDomains))
	for _, d := range autoAllowDomains {
		doms[d] = struct{}{}
	}
	return &PgAllowlist{pool: pool, autoAllowDoms: doms}
}

// compile-time assertion that PgAllowlist satisfies the Allowlist contract.
var _ onboarding.Allowlist = (*PgAllowlist)(nil)

// pgDomainOf returns the lowercased substring after the last '@', or "" when
// no '@' is present. Mirrors onboarding.domainOf; kept package-private here so
// both impls share the same extraction logic without exporting the helper.
func pgDomainOf(canonicalEmail string) string {
	i := strings.LastIndex(canonicalEmail, "@")
	if i < 0 {
		return ""
	}
	return strings.ToLower(canonicalEmail[i+1:])
}

// IsAllowed reports whether a canonical email may provision. The auto-allow
// domain check runs in Go first; only the exact-row lookup hits Postgres.
// Never logs the email.
func (p *PgAllowlist) IsAllowed(ctx context.Context, canonicalEmail string) (bool, error) {
	if _, ok := p.autoAllowDoms[pgDomainOf(canonicalEmail)]; ok {
		return true, nil
	}
	const q = `SELECT 1 FROM allowlist WHERE email = $1`
	var one int
	err := p.pool.QueryRow(ctx, q, canonicalEmail).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("is allowed: %w", err)
	}
	return true, nil
}

// Add idempotently inserts a row for the canonical email. A second Add for the
// same email is a silent no-op (ON CONFLICT DO NOTHING). Never logs the email.
func (p *PgAllowlist) Add(ctx context.Context, canonicalEmail, note string, now time.Time) error {
	const q = `INSERT INTO allowlist (email, note, created_at) VALUES ($1, $2, $3) ON CONFLICT (email) DO NOTHING`
	_, err := p.pool.Exec(ctx, q, canonicalEmail, note, now)
	if err != nil {
		return fmt.Errorf("add to allowlist: %w", err)
	}
	return nil
}
