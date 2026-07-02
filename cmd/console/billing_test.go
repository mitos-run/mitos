package main

import (
	"context"
	"errors"
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

// TestPortalLinkerResolvesOrgToCustomer asserts the adapter maps org to
// customer (via OrgCustomers) and then asks the provider for that customer's
// portal URL.
func TestPortalLinkerResolvesOrgToCustomer(t *testing.T) {
	customers := billingprovider.NewMemCustomers()
	if err := customers.Link(context.Background(), "org-alice", "cus_alice"); err != nil {
		t.Fatalf("Link: %v", err)
	}
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

// errOrgCustomers is an OrgCustomers whose store is down: every lookup errors.
type errOrgCustomers struct{}

func (errOrgCustomers) CustomerForOrg(context.Context, string) (string, bool, error) {
	return "", false, errors.New("store unavailable")
}

// TestPortalLinkerStoreFailureIsNotNotFound asserts a customer-store FAILURE
// surfaces as an error distinct from console.ErrNotFound, so the BFF answers
// 5xx instead of quietly hiding the portal behind a 404 (issue #614: a durable
// store can fail transiently; a mem map could not).
func TestPortalLinkerStoreFailureIsNotNotFound(t *testing.T) {
	pl := portalLinker{provider: stubProvider{}, customers: errOrgCustomers{}}
	_, err := pl.PortalURL(context.Background(), "org-alice")
	if err == nil || errors.Is(err, console.ErrNotFound) {
		t.Fatalf("err = %v, want a non-ErrNotFound failure", err)
	}
}
