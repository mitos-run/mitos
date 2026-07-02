package pgstore_test

import (
	"context"
	"testing"

	"mitos.run/mitos/internal/saas/pgstore"
)

func TestPgCustomers(t *testing.T) {
	dsn := testDSN(t)
	pg, err := pgstore.Open(context.Background(), dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(pg.Close)
	truncateTables(t, dsn, "billing_customers")
	c := pgstore.NewPgCustomers(pg.Pool())
	ctx := context.Background()

	// Unknown lookups are (empty, false, nil) in both directions.
	if org, ok, err := c.OrgForCustomer(ctx, "cus_nope"); err != nil || ok || org != "" {
		t.Fatalf("OrgForCustomer unknown = %q,%v,%v; want empty,false,nil", org, ok, err)
	}
	if cust, ok, err := c.CustomerForOrg(ctx, "org-nope"); err != nil || ok || cust != "" {
		t.Fatalf("CustomerForOrg unknown = %q,%v,%v; want empty,false,nil", cust, ok, err)
	}

	// Link resolves both directions (the webhook needs customer to org, the
	// portal and top-up links need org to customer).
	if err := c.Link(ctx, "org-alice", "cus_alice"); err != nil {
		t.Fatalf("Link: %v", err)
	}
	if org, ok, err := c.OrgForCustomer(ctx, "cus_alice"); err != nil || !ok || org != "org-alice" {
		t.Fatalf("OrgForCustomer = %q,%v,%v; want org-alice,true,nil", org, ok, err)
	}
	if cust, ok, err := c.CustomerForOrg(ctx, "org-alice"); err != nil || !ok || cust != "cus_alice" {
		t.Fatalf("CustomerForOrg = %q,%v,%v; want cus_alice,true,nil", cust, ok, err)
	}

	// Relinking the SAME pair is idempotent (webhooks replay).
	if err := c.Link(ctx, "org-alice", "cus_alice"); err != nil {
		t.Fatalf("Link replay: %v", err)
	}
	if org, ok, err := c.OrgForCustomer(ctx, "cus_alice"); err != nil || !ok || org != "org-alice" {
		t.Fatalf("OrgForCustomer after replay = %q,%v,%v; want org-alice,true,nil", org, ok, err)
	}

	// Relinking the org to a NEW customer replaces the old link in both
	// directions: the stale customer no longer resolves.
	if err := c.Link(ctx, "org-alice", "cus_alice2"); err != nil {
		t.Fatalf("Link new customer: %v", err)
	}
	if cust, ok, err := c.CustomerForOrg(ctx, "org-alice"); err != nil || !ok || cust != "cus_alice2" {
		t.Fatalf("CustomerForOrg after relink = %q,%v,%v; want cus_alice2,true,nil", cust, ok, err)
	}
	if _, ok, err := c.OrgForCustomer(ctx, "cus_alice"); err != nil || ok {
		t.Fatalf("stale customer still resolves (ok=%v, err=%v)", ok, err)
	}

	// Relinking the customer to a NEW org replaces the old link too (last write
	// wins deterministically on either side; no unique-violation error).
	if err := c.Link(ctx, "org-bob", "cus_alice2"); err != nil {
		t.Fatalf("Link new org: %v", err)
	}
	if org, ok, err := c.OrgForCustomer(ctx, "cus_alice2"); err != nil || !ok || org != "org-bob" {
		t.Fatalf("OrgForCustomer after org relink = %q,%v,%v; want org-bob,true,nil", org, ok, err)
	}
	if _, ok, err := c.CustomerForOrg(ctx, "org-alice"); err != nil || ok {
		t.Fatalf("stale org still resolves (ok=%v, err=%v)", ok, err)
	}

	// Restart survival: a brand-new store over a brand-new pool still resolves
	// the mapping, so a webhook arriving after a console restart can map its
	// customer_id back to the org (issue #614).
	pg2, err := pgstore.Open(context.Background(), dsn)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(pg2.Close)
	c2 := pgstore.NewPgCustomers(pg2.Pool())
	if org, ok, err := c2.OrgForCustomer(ctx, "cus_alice2"); err != nil || !ok || org != "org-bob" {
		t.Fatalf("OrgForCustomer after restart = %q,%v,%v; want org-bob,true,nil (mapping lost on restart)", org, ok, err)
	}
}
