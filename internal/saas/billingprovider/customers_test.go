package billingprovider

import (
	"context"
	"testing"
)

// TestMemCustomersBidirectional asserts the in-memory map resolves both
// directions (the webhook needs customer to org, the portal needs org to
// customer) and reports unknowns as not-found without error.
func TestMemCustomersBidirectional(t *testing.T) {
	m := NewMemCustomers()
	if err := m.Link(context.Background(), "org-alice", "cus_alice"); err != nil {
		t.Fatalf("Link: %v", err)
	}

	if org, ok, err := m.OrgForCustomer(context.Background(), "cus_alice"); err != nil || !ok || org != "org-alice" {
		t.Fatalf("OrgForCustomer = %q,%v,%v; want org-alice,true,nil", org, ok, err)
	}
	if cust, ok, err := m.CustomerForOrg(context.Background(), "org-alice"); err != nil || !ok || cust != "cus_alice" {
		t.Fatalf("CustomerForOrg = %q,%v,%v; want cus_alice,true,nil", cust, ok, err)
	}
	if _, ok, err := m.OrgForCustomer(context.Background(), "cus_nope"); err != nil || ok {
		t.Fatalf("unknown customer resolved (ok=%v, err=%v)", ok, err)
	}
	if _, ok, err := m.CustomerForOrg(context.Background(), "org-nope"); err != nil || ok {
		t.Fatalf("unknown org resolved (ok=%v, err=%v)", ok, err)
	}
}

// TestMemCustomersRelinkReplacesBothDirections asserts a relink of either side
// removes the stale inverse entry (last write wins), matching the durable
// store's semantics so mem and pg are interchangeable.
func TestMemCustomersRelinkReplacesBothDirections(t *testing.T) {
	m := NewMemCustomers()
	ctx := context.Background()
	if err := m.Link(ctx, "org-alice", "cus_1"); err != nil {
		t.Fatalf("Link: %v", err)
	}
	// Same pair again: idempotent.
	if err := m.Link(ctx, "org-alice", "cus_1"); err != nil {
		t.Fatalf("Link replay: %v", err)
	}
	// Org moves to a new customer: the stale customer no longer resolves.
	if err := m.Link(ctx, "org-alice", "cus_2"); err != nil {
		t.Fatalf("Link new customer: %v", err)
	}
	if _, ok, _ := m.OrgForCustomer(ctx, "cus_1"); ok {
		t.Fatal("stale customer still resolves after relink")
	}
	// Customer moves to a new org: the stale org no longer resolves.
	if err := m.Link(ctx, "org-bob", "cus_2"); err != nil {
		t.Fatalf("Link new org: %v", err)
	}
	if _, ok, _ := m.CustomerForOrg(ctx, "org-alice"); ok {
		t.Fatal("stale org still resolves after relink")
	}
	if org, ok, _ := m.OrgForCustomer(ctx, "cus_2"); !ok || org != "org-bob" {
		t.Fatalf("OrgForCustomer = %q,%v; want org-bob,true", org, ok)
	}
}
