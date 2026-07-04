package billingprovider

import (
	"errors"
	"net/http"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"mitos.run/mitos/internal/saas/billing"
)

// TestWebhookMetricsCountsVerifyFailure asserts a forged/replayed webhook (a
// provider verification error) increments the verify-failure counter and NOT
// the handler-error counter: the 400 is the correct refusal, not a fault.
func TestWebhookMetricsCountsVerifyFailure(t *testing.T) {
	m := NewWebhookMetrics()
	m.MustRegister(prometheus.NewRegistry())
	h := NewWebhookHandler(fakeProvider{err: errors.New("bad signature")},
		fakeCustomers{}, nil, billing.NewMemStatusStore(), nil, nil).WithMetrics(m)
	w := post(h)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
	if got := testutil.ToFloat64(m.verifyFailures); got != 1 {
		t.Errorf("verify failures = %v, want 1", got)
	}
	if got := testutil.ToFloat64(m.handlerErrors); got != 0 {
		t.Errorf("handler errors = %v, want 0", got)
	}
}

// TestWebhookMetricsCountsHandlerError asserts a store failure behind a
// verified event (here: the customer-link write) increments the handler-error
// counter: the provider will retry, and sustained failures mean top-ups and
// status syncs are not landing.
func TestWebhookMetricsCountsHandlerError(t *testing.T) {
	m := NewWebhookMetrics()
	m.MustRegister(prometheus.NewRegistry())
	h := NewWebhookHandler(
		fakeProvider{ev: Event{OrgID: "org-alice", CustomerRef: "cus_alice"}},
		fakeCustomers{}, errLinker{}, billing.NewMemStatusStore(), nil, nil).WithMetrics(m)
	w := post(h)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", w.Code)
	}
	if got := testutil.ToFloat64(m.handlerErrors); got != 1 {
		t.Errorf("handler errors = %v, want 1", got)
	}
	if got := testutil.ToFloat64(m.verifyFailures); got != 0 {
		t.Errorf("verify failures = %v, want 0", got)
	}
}

// TestWebhookMetricsSuccessCountsNothing asserts a cleanly applied event moves
// neither counter, and that a handler without metrics (every existing caller)
// still serves.
func TestWebhookMetricsSuccessCountsNothing(t *testing.T) {
	m := NewWebhookMetrics()
	m.MustRegister(prometheus.NewRegistry())
	h := NewWebhookHandler(
		fakeProvider{ev: Event{Status: billing.StatusPastDue, CustomerRef: "cus_alice"}},
		fakeCustomers{"cus_alice": "org-alice"}, nil, billing.NewMemStatusStore(), nil, nil).WithMetrics(m)
	if w := post(h); w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if got := testutil.ToFloat64(m.verifyFailures) + testutil.ToFloat64(m.handlerErrors); got != 0 {
		t.Errorf("counters moved on success: %v", got)
	}
	// No metrics wired: must not panic.
	bare := NewWebhookHandler(fakeProvider{err: errors.New("bad signature")},
		fakeCustomers{}, nil, billing.NewMemStatusStore(), nil, nil)
	if w := post(bare); w.Code != http.StatusBadRequest {
		t.Fatalf("bare handler status = %d, want 400", w.Code)
	}
}
