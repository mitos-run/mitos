package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
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

// stubCheckout is a checkoutCreator returning a fixed Checkout, recording the
// TopUp it was given.
type stubCheckout struct {
	co  billingprovider.Checkout
	err error
	got billingprovider.TopUp
}

func (s *stubCheckout) CreateCheckout(_ context.Context, in billingprovider.TopUp) (billingprovider.Checkout, error) {
	s.got = in
	return s.co, s.err
}

// errLinker is a CustomerLinker whose store is down: every write errors.
type errLinker struct{}

func (errLinker) Link(context.Context, string, string) error {
	return errors.New("store unavailable")
}

// countingLinker records Link calls on top of a MemCustomers.
type countingLinker struct {
	*billingprovider.MemCustomers
	calls int
}

func (c *countingLinker) Link(ctx context.Context, orgID, customerRef string) error {
	c.calls++
	return c.MemCustomers.Link(ctx, orgID, customerRef)
}

// TestTopUpLinkerRecordsResponseCustomer asserts the checkout-time link (best
// effort, issue #618): when the provider's create-transaction response names a
// customer that differs from the stored link, the fresh ref is recorded and the
// checkout URL is still returned.
func TestTopUpLinkerRecordsResponseCustomer(t *testing.T) {
	ctx := context.Background()
	customers := billingprovider.NewMemCustomers()
	if err := customers.Link(ctx, "org-alice", "ctm_old"); err != nil {
		t.Fatalf("Link: %v", err)
	}
	provider := &stubCheckout{co: billingprovider.Checkout{URL: "https://pay/txn_1", CustomerRef: "ctm_new"}}
	tl := topUpLinker{provider: provider, customers: customers, linker: customers, logger: discardLogger()}

	url, err := tl.CheckoutURL(ctx, billingprovider.TopUp{OrgID: "org-alice", AmountCents: 5000})
	if err != nil {
		t.Fatalf("CheckoutURL: %v", err)
	}
	if url != "https://pay/txn_1" {
		t.Fatalf("url = %q", url)
	}
	if provider.got.CustomerRef != "ctm_old" {
		t.Fatalf("provider got CustomerRef %q, want the stored ctm_old", provider.got.CustomerRef)
	}
	got, ok, err := customers.CustomerForOrg(ctx, "org-alice")
	if err != nil || !ok || got != "ctm_new" {
		t.Fatalf("stored customer = (%q, %v, %v), want (ctm_new, true, nil)", got, ok, err)
	}
}

// TestTopUpLinkerNoRelinkWhenUnchanged asserts the checkout-time link is not
// rewritten when the response names the same customer we already store.
func TestTopUpLinkerNoRelinkWhenUnchanged(t *testing.T) {
	ctx := context.Background()
	linker := &countingLinker{MemCustomers: billingprovider.NewMemCustomers()}
	if err := linker.MemCustomers.Link(ctx, "org-alice", "ctm_same"); err != nil {
		t.Fatalf("Link: %v", err)
	}
	provider := &stubCheckout{co: billingprovider.Checkout{URL: "https://pay/txn_2", CustomerRef: "ctm_same"}}
	tl := topUpLinker{provider: provider, customers: linker.MemCustomers, linker: linker, logger: discardLogger()}
	if _, err := tl.CheckoutURL(ctx, billingprovider.TopUp{OrgID: "org-alice", AmountCents: 100}); err != nil {
		t.Fatalf("CheckoutURL: %v", err)
	}
	if linker.calls != 0 {
		t.Fatalf("Link called %d times for an unchanged customer, want 0", linker.calls)
	}
}

// TestTopUpLinkerLinkFailureDoesNotFailCheckout asserts a checkout-time link
// FAILURE is best effort: the buyer still gets the checkout URL (the
// webhook-time link is the reliable path).
func TestTopUpLinkerLinkFailureDoesNotFailCheckout(t *testing.T) {
	ctx := context.Background()
	customers := billingprovider.NewMemCustomers()
	if err := customers.Link(ctx, "org-alice", "ctm_old"); err != nil {
		t.Fatalf("Link: %v", err)
	}
	provider := &stubCheckout{co: billingprovider.Checkout{URL: "https://pay/txn_3", CustomerRef: "ctm_new"}}
	tl := topUpLinker{provider: provider, customers: customers, linker: errLinker{}, logger: discardLogger()}
	url, err := tl.CheckoutURL(ctx, billingprovider.TopUp{OrgID: "org-alice", AmountCents: 100})
	if err != nil {
		t.Fatalf("CheckoutURL failed on a best-effort link error: %v", err)
	}
	if url != "https://pay/txn_3" {
		t.Fatalf("url = %q", url)
	}
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
