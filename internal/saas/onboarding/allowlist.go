package onboarding

import (
	"context"
	"strings"
	"sync"
	"time"
)

// Allowlist reports whether a canonical email may provision a new account.
type Allowlist interface {
	// IsAllowed reports whether a canonical email may provision. Precedence: an
	// auto-allow domain match first, else an exact allowlist row. Never logs the email.
	IsAllowed(ctx context.Context, canonicalEmail string) (bool, error)
	// Add idempotently inserts an allowlist row for a canonical email with an
	// optional non-secret note.
	Add(ctx context.Context, canonicalEmail, note string, now time.Time) error
}

// domainOf returns the lowercased substring after the last '@', or "" when no
// '@' is present. Used by MemAllowlist for auto-allow domain checks. PgAllowlist
// (in the pgstore package) duplicates this extraction so both impls share the
// same precedence logic without crossing the package boundary.
func domainOf(canonicalEmail string) string {
	i := strings.LastIndex(canonicalEmail, "@")
	if i < 0 {
		return ""
	}
	return strings.ToLower(canonicalEmail[i+1:])
}

// MemAllowlist is an in-memory Allowlist suitable for tests and local dev.
// It is the behavioral reference for PgAllowlist.
type MemAllowlist struct {
	mu            sync.Mutex
	autoAllowDoms map[string]struct{}
	rows          map[string]struct{}
}

// NewMemAllowlist returns a MemAllowlist seeded with auto-allow domains.
// Each entry must be already lowercased with no leading '@'.
func NewMemAllowlist(autoAllowDomains []string) *MemAllowlist {
	doms := make(map[string]struct{}, len(autoAllowDomains))
	for _, d := range autoAllowDomains {
		doms[d] = struct{}{}
	}
	return &MemAllowlist{
		autoAllowDoms: doms,
		rows:          make(map[string]struct{}),
	}
}

// IsAllowed reports whether a canonical email may provision. The auto-allow
// domain check takes precedence over an exact row. Never logs the email.
func (m *MemAllowlist) IsAllowed(_ context.Context, canonicalEmail string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.autoAllowDoms[domainOf(canonicalEmail)]; ok {
		return true, nil
	}
	_, ok := m.rows[canonicalEmail]
	return ok, nil
}

// Add idempotently adds a row for the canonical email. A second Add for the
// same email is a silent no-op.
func (m *MemAllowlist) Add(_ context.Context, canonicalEmail, _ string, _ time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rows[canonicalEmail] = struct{}{}
	return nil
}
