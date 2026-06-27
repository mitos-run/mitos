package pgstore

import (
	"github.com/jackc/pgx/v5/pgxpool"
	"mitos.run/mitos/internal/saas"
	"mitos.run/mitos/internal/saas/billing"
	"mitos.run/mitos/internal/saas/onboarding"
)

// OnboardingStores returns the durable credit ledger, pending store, and session
// store backed by the given pool. Callers pass a non-nil pool only when Postgres
// is configured; otherwise they use the in-memory constructors directly.
func OnboardingStores(pool *pgxpool.Pool) (billing.CreditLedger, onboarding.PendingStore, saas.Sessions) {
	return NewPgCreditLedger(pool), NewPgPendingStore(pool), NewPgSessionStore(pool)
}
