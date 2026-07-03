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

func (f fakeCustomers) OrgForCustomer(_ context.Context, c string) (string, bool, error) {
	o, ok := f[c]
	return o, ok, nil
}

// errCustomers is a CustomerResolver whose store is down: every lookup errors.
type errCustomers struct{}

func (errCustomers) OrgForCustomer(context.Context, string) (string, bool, error) {
	return "", false, errors.New("store unavailable")
}

// errLinker is a CustomerLinker whose store is down: every write errors.
type errLinker struct{}

func (errLinker) Link(context.Context, string, string) error {
	return errors.New("store unavailable")
}

func post(h http.Handler) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", "/webhooks/billing", strings.NewReader("{}"))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func handle(t *testing.T, p Provider) (*httptest.ResponseRecorder, *billing.MemStatusStore) {
	t.Helper()
	status := billing.NewMemStatusStore()
	h := NewWebhookHandler(p, fakeCustomers{"cus_alice": "org-alice"}, nil, status, nil, nil)
	return post(h), status
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

// TestCustomerLookupFailureIsRetried asserts a customer-store FAILURE (as
// opposed to an unknown customer) is a 5xx, so the provider retries and a
// transient store error cannot permanently drop a status sync (issue #614).
func TestCustomerLookupFailureIsRetried(t *testing.T) {
	status := billing.NewMemStatusStore()
	h := NewWebhookHandler(fakeProvider{ev: Event{Status: billing.StatusSuspended, CustomerRef: "cus_alice"}}, errCustomers{}, nil, status, nil, nil)
	w := post(h)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (a lookup failure must not be acked)", w.Code)
	}
	got, _ := status.Status(context.Background(), "org-alice")
	if got != billing.StatusActive {
		t.Fatalf("failed lookup changed status to %q", got)
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

// TestWebhookLinksCustomerFromCustomData asserts the write half of #614: an
// event whose signature-verified custom_data names the org AND whose envelope
// names the provider customer records the link BEFORE processing, so the very
// first event for a new customer both establishes the mapping and applies its
// own status; a later event that lacks custom_data (subscription.canceled
// etc.) then resolves the org through that recorded link.
func TestWebhookLinksCustomerFromCustomData(t *testing.T) {
	ctx := context.Background()
	customers := NewMemCustomers()
	status := billing.NewMemStatusStore()

	// First event: brand-new customer, org known only from custom_data.
	first := NewWebhookHandler(fakeProvider{ev: Event{Status: billing.StatusPastDue, CustomerRef: "ctm_new", OrgID: "org-new"}}, customers, customers, status, nil, nil)
	if w := post(first); w.Code != http.StatusOK {
		t.Fatalf("first event status = %d, want 200", w.Code)
	}
	org, ok, err := customers.OrgForCustomer(ctx, "ctm_new")
	if err != nil || !ok || org != "org-new" {
		t.Fatalf("link after first event = (%q, %v, %v), want (org-new, true, nil)", org, ok, err)
	}
	if got, _ := status.Status(ctx, "org-new"); got != billing.StatusPastDue {
		t.Fatalf("first event did not apply its own status: got %q, want past_due", got)
	}

	// Second event: same customer, NO custom_data (OrgID empty). It must
	// resolve through the link the first event recorded.
	second := NewWebhookHandler(fakeProvider{ev: Event{Status: billing.StatusSuspended, CustomerRef: "ctm_new"}}, customers, customers, status, nil, nil)
	if w := post(second); w.Code != http.StatusOK {
		t.Fatalf("second event status = %d, want 200", w.Code)
	}
	if got, _ := status.Status(ctx, "org-new"); got != billing.StatusSuspended {
		t.Fatalf("second event did not resolve via the recorded link: got %q, want suspended", got)
	}
}

// TestWebhookLinkIdempotentReplay asserts a redelivered event relinks the same
// pair without error: both deliveries are 200 and the mapping is unchanged.
func TestWebhookLinkIdempotentReplay(t *testing.T) {
	ctx := context.Background()
	customers := NewMemCustomers()
	status := billing.NewMemStatusStore()
	h := NewWebhookHandler(fakeProvider{ev: Event{Status: billing.StatusActive, CustomerRef: "ctm_r", OrgID: "org-r"}}, customers, customers, status, nil, nil)
	for i := 0; i < 2; i++ {
		if w := post(h); w.Code != http.StatusOK {
			t.Fatalf("delivery %d status = %d, want 200", i+1, w.Code)
		}
	}
	org, ok, err := customers.OrgForCustomer(ctx, "ctm_r")
	if err != nil || !ok || org != "org-r" {
		t.Fatalf("link after replay = (%q, %v, %v), want (org-r, true, nil)", org, ok, err)
	}
}

// TestWebhookLinkFailureIsRetried asserts a link-store FAILURE is a 5xx (the
// provider retries; the event is never dropped silently) and no billing state
// is mutated, matching the #614 lookup-failure posture.
func TestWebhookLinkFailureIsRetried(t *testing.T) {
	status := billing.NewMemStatusStore()
	h := NewWebhookHandler(fakeProvider{ev: Event{Status: billing.StatusSuspended, CustomerRef: "cus_alice", OrgID: "org-alice"}}, fakeCustomers{"cus_alice": "org-alice"}, errLinker{}, status, nil, nil)
	w := post(h)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (a link failure must not be acked)", w.Code)
	}
	got, _ := status.Status(context.Background(), "org-alice")
	if got != billing.StatusActive {
		t.Fatalf("failed link still changed status to %q", got)
	}
}

// TestWebhookNilLinkerSkipsLinking asserts a handler wired without a linker
// (community edition or older wiring) processes an OrgID-carrying event as
// before: the unknown customer is acked, nothing crashes.
func TestWebhookNilLinkerSkipsLinking(t *testing.T) {
	status := billing.NewMemStatusStore()
	h := NewWebhookHandler(fakeProvider{ev: Event{Status: billing.StatusSuspended, CustomerRef: "ctm_x", OrgID: "org-x"}}, fakeCustomers{}, nil, status, nil, nil)
	if w := post(h); w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 ack", w.Code)
	}
}
