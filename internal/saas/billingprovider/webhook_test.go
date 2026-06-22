package billingprovider

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"mitos.run/mitos/internal/saas/billing"
)

// fakeProvider is a stand-in billing provider: it returns a fixed normalized
// event or a verification error, so the generic handler is tested independent of
// any provider's signature scheme.
type fakeProvider struct {
	ev  Event
	err error
}

func (f fakeProvider) Name() string { return "fake" }
func (f fakeProvider) VerifyWebhook(_ *http.Request, _ []byte) (Event, error) {
	return f.ev, f.err
}
func (f fakeProvider) PortalURL(_ context.Context, _ string) (string, error) {
	return "https://billing.example/portal", nil
}

type fakeCustomers map[string]string

func (f fakeCustomers) OrgForCustomer(_ context.Context, c string) (string, bool) {
	o, ok := f[c]
	return o, ok
}

func handle(t *testing.T, p Provider) (*httptest.ResponseRecorder, *billing.MemStatusStore) {
	t.Helper()
	status := billing.NewMemStatusStore()
	h := NewWebhookHandler(p, fakeCustomers{"cus_alice": "org-alice"}, status)
	r := httptest.NewRequest("POST", "/webhooks/billing", strings.NewReader("{}"))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w, status
}

// TestProviderEventMapsToStatus asserts a normalized event from ANY provider is
// applied to the org's billing status — the dunning/status core is
// provider-neutral.
func TestProviderEventMapsToStatus(t *testing.T) {
	w, status := handle(t, fakeProvider{ev: Event{Status: billing.StatusPastDue, CustomerRef: "cus_alice"}})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	got, _ := status.Status(context.Background(), "org-alice")
	if got != billing.StatusPastDue {
		t.Fatalf("status = %q, want past_due", got)
	}
}

// TestVerificationFailureRejected asserts a provider verification error is a 400
// with no status change (forged/replayed webhook).
func TestVerificationFailureRejected(t *testing.T) {
	w, status := handle(t, fakeProvider{err: errors.New("bad signature")})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
	got, _ := status.Status(context.Background(), "org-alice")
	if got != billing.StatusActive {
		t.Fatalf("forged webhook changed status to %q", got)
	}
}

// TestUnknownCustomerAcknowledged asserts an event for an unmapped customer is
// acked (2xx) without action, so the provider stops retrying.
func TestUnknownCustomerAcknowledged(t *testing.T) {
	w, _ := handle(t, fakeProvider{ev: Event{Status: billing.StatusSuspended, CustomerRef: "cus_unknown"}})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 ack", w.Code)
	}
}

// TestIgnoredEventAcknowledged asserts an event the provider couldn't map to a
// status (empty Status) is acked without a status change.
func TestIgnoredEventAcknowledged(t *testing.T) {
	w, status := handle(t, fakeProvider{ev: Event{Status: "", CustomerRef: "cus_alice"}})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 ack", w.Code)
	}
	got, _ := status.Status(context.Background(), "org-alice")
	if got != billing.StatusActive {
		t.Fatalf("ignored event changed status to %q", got)
	}
}
