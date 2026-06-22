package console

import (
	"context"
	"net/http"
	"testing"
)

// fakePortal returns a fixed URL for one org and ErrNotFound otherwise.
type fakePortal struct{ org, url string }

func (f fakePortal) PortalURL(_ context.Context, orgID string) (string, error) {
	if orgID == f.org {
		return f.url, nil
	}
	return "", ErrNotFound
}

// TestBillingPortalReturnsURLForOrg asserts the endpoint returns the caller
// org's manage-subscription URL from the provider-neutral PortalLinker seam.
func TestBillingPortalReturnsURLForOrg(t *testing.T) {
	f := newFixture(t)
	f.con = New(Deps{Accounts: f.accounts, Portal: fakePortal{org: f.aliceOrg, url: "https://billing.example/portal/abc"}})
	w := f.req(t, "GET", "/console/billing/portal", "", f.aliceAcct, f.aliceOrg)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		URL string `json:"url"`
	}
	decode(t, w, &resp)
	if resp.URL != "https://billing.example/portal/abc" {
		t.Fatalf("url = %q, want the portal URL", resp.URL)
	}
}

// TestBillingPortalUnavailableIs404 asserts that when no portal is configured
// (the default, e.g. community edition), the endpoint is a 404 rather than a
// fabricated link.
func TestBillingPortalUnavailableIs404(t *testing.T) {
	f := newFixture(t) // default Portal seam → not configured
	w := f.req(t, "GET", "/console/billing/portal", "", f.aliceAcct, f.aliceOrg)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 when no portal is configured", w.Code)
	}
}
