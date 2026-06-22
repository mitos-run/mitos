package main

import (
	"context"
	"net/http"
	"testing"

	"mitos.run/mitos/internal/saas/billingprovider"
	"mitos.run/mitos/internal/saas/console"
)

// stubProvider is a billingprovider.Provider whose PortalURL echoes the customer
// ref, so the adapter's org→customer→URL composition is observable.
type stubProvider struct{}

func (stubProvider) Name() string { return "stub" }
func (stubProvider) VerifyWebhook(*http.Request, []byte) (billingprovider.Event, error) {
	return billingprovider.Event{}, nil
}
func (stubProvider) PortalURL(_ context.Context, customerRef string) (string, error) {
	return "https://portal/" + customerRef, nil
}

// TestPortalLinkerResolvesOrgToCustomer asserts the adapter maps org→customer
// (via OrgCustomers) and then asks the provider for that customer's portal URL.
func TestPortalLinkerResolvesOrgToCustomer(t *testing.T) {
	customers := billingprovider.NewMemCustomers()
	customers.Link("org-alice", "cus_alice")
	pl := portalLinker{provider: stubProvider{}, customers: customers}

	url, err := pl.PortalURL(context.Background(), "org-alice")
	if err != nil {
		t.Fatalf("PortalURL: %v", err)
	}
	if url != "https://portal/cus_alice" {
		t.Fatalf("url = %q, want https://portal/cus_alice", url)
	}
}

// TestPortalLinkerUnknownOrgIsNotFound asserts an org with no linked customer is
// reported as not-found (the BFF turns this into a 404).
func TestPortalLinkerUnknownOrgIsNotFound(t *testing.T) {
	pl := portalLinker{provider: stubProvider{}, customers: billingprovider.NewMemCustomers()}
	if _, err := pl.PortalURL(context.Background(), "org-nope"); err != console.ErrNotFound {
		t.Fatalf("err = %v, want console.ErrNotFound", err)
	}
}
