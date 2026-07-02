package pgstore

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"mitos.run/mitos/internal/saas/billingprovider"
)

// PgCustomers is the durable bidirectional org <-> provider-customer map. It
// satisfies both billingprovider.CustomerResolver (customer to org, for the
// webhook) and billingprovider.OrgCustomers (org to customer, for the portal
// and top-up links). Durability is the point (issue #614): after a console
// restart the webhook handler must still map an incoming customer_id back to
// its org, or status syncs and paid credit top-ups are dropped.
type PgCustomers struct {
	pool *pgxpool.Pool
}

// NewPgCustomers returns a PgCustomers backed by pool.
func NewPgCustomers(pool *pgxpool.Pool) *PgCustomers {
	return &PgCustomers{pool: pool}
}

// compile-time assertion that PgCustomers satisfies the full customers seam.
var _ billingprovider.Customers = (*PgCustomers)(nil)

// Link records the org <-> customer association. It is idempotent: relinking
// the same pair is a no-op, and a relink of either side replaces the stale row
// (last write wins deterministically in both directions). The delete and
// insert commit together so a replayed webhook or a concurrent relink never
// leaves a half-updated mapping.
func (c *PgCustomers) Link(ctx context.Context, orgID, customerRef string) error {
	tx, err := c.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("link billing customer: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx,
		`DELETE FROM billing_customers WHERE org_id = $1 OR customer_ref = $2`,
		orgID, customerRef); err != nil {
		return fmt.Errorf("link billing customer: clear stale links: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO billing_customers (org_id, customer_ref, linked_at) VALUES ($1, $2, now())`,
		orgID, customerRef); err != nil {
		return fmt.Errorf("link billing customer: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("link billing customer: commit: %w", err)
	}
	return nil
}

// OrgForCustomer implements billingprovider.CustomerResolver. An unknown
// customer is ("", false, nil); a store failure is an error so the webhook
// answers 5xx and the provider retries instead of dropping the event.
func (c *PgCustomers) OrgForCustomer(ctx context.Context, customerRef string) (string, bool, error) {
	const q = `SELECT org_id FROM billing_customers WHERE customer_ref = $1`
	var org string
	err := c.pool.QueryRow(ctx, q, customerRef).Scan(&org)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("org for billing customer: %w", err)
	}
	return org, true, nil
}

// CustomerForOrg implements billingprovider.OrgCustomers. An org with no
// linked customer is ("", false, nil).
func (c *PgCustomers) CustomerForOrg(ctx context.Context, orgID string) (string, bool, error) {
	const q = `SELECT customer_ref FROM billing_customers WHERE org_id = $1`
	var cust string
	err := c.pool.QueryRow(ctx, q, orgID).Scan(&cust)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("customer for org: %w", err)
	}
	return cust, true, nil
}
