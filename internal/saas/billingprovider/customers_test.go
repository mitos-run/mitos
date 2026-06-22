package billingprovider

import (
	"context"
	"testing"
)

// TestMemCustomersBidirectional asserts the in-memory map resolves both
// directions (the webhook needs customer→org, the portal needs org→customer)
// and reports unknowns as not-found.
func TestMemCustomersBidirectional(t *testing.T) {
	m := NewMemCustomers()
	m.Link("org-alice", "cus_alice")

	if org, ok := m.OrgForCustomer(context.Background(), "cus_alice"); !ok || org != "org-alice" {
		t.Fatalf("OrgForCustomer = %q,%v; want org-alice,true", org, ok)
	}
	if cust, ok := m.CustomerForOrg(context.Background(), "org-alice"); !ok || cust != "cus_alice" {
		t.Fatalf("CustomerForOrg = %q,%v; want cus_alice,true", cust, ok)
	}
	if _, ok := m.OrgForCustomer(context.Background(), "cus_nope"); ok {
		t.Fatal("unknown customer resolved")
	}
	if _, ok := m.CustomerForOrg(context.Background(), "org-nope"); ok {
		t.Fatal("unknown org resolved")
	}
}
