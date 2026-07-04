package billingprovider

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"mitos.run/mitos/internal/saas/billing"
)

// recordingSuspender records billing.Suspender calls for the webhook tests.
type recordingSuspender struct {
	calls []suspendCall
	err   error
}

type suspendCall struct {
	orgID, reason, note string
	manualHold          bool
}

func (s *recordingSuspender) Suspend(_ context.Context, orgID, reason, note string, manualHold bool) error {
	if s.err != nil {
		return s.err
	}
	s.calls = append(s.calls, suspendCall{orgID, reason, note, manualHold})
	return nil
}

// TestWebhookSuspendedStatusFiresSuspender asserts a provider event carrying
// the suspended status (subscription canceled/paused, payment retries
// exhausted) drives the kill-switch through the Suspender seam on the
// transition INTO suspended, so the org fails closed at the gateway instead of
// only its billing status flipping (issue #615: previously the provider
// webhook set the status and no suspension reached any store).
func TestWebhookSuspendedStatusFiresSuspender(t *testing.T) {
	ctx := context.Background()
	status := billing.NewMemStatusStore()
	sus := &recordingSuspender{}
	h := NewWebhookHandler(fakeProvider{ev: Event{Status: billing.StatusSuspended, CustomerRef: "cus_alice"}},
		fakeCustomers{"cus_alice": "org-alice"}, nil, status, nil, nil).WithSuspender(sus)

	w := post(h)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}
	if len(sus.calls) != 1 {
		t.Fatalf("suspender calls = %d, want 1", len(sus.calls))
	}
	call := sus.calls[0]
	if call.orgID != "org-alice" || call.reason != "dunning" || call.manualHold {
		t.Errorf("suspend call = %+v, want org-alice/dunning without a manual hold", call)
	}
	got, _ := status.Status(ctx, "org-alice")
	if got != billing.StatusSuspended {
		t.Errorf("billing status = %q, want suspended", got)
	}

	// Redelivery of the same event: the org is ALREADY suspended, so the
	// transition gate keeps the suspender quiet (the suspension time in the
	// store must not be churned by webhook retries).
	w = post(h)
	if w.Code != http.StatusOK {
		t.Fatalf("redelivery status = %d, want 200", w.Code)
	}
	if len(sus.calls) != 1 {
		t.Errorf("suspender calls after redelivery = %d, want still 1 (transition-gated)", len(sus.calls))
	}
}

// TestWebhookSuspendFailureIsRetried asserts a suspender write failure is a
// 5xx BEFORE the status is applied: the provider retries the event, so a
// transient kill-switch store outage can neither drop the suspension nor leave
// the billing status claiming suspended while the gateway still admits the org.
func TestWebhookSuspendFailureIsRetried(t *testing.T) {
	ctx := context.Background()
	status := billing.NewMemStatusStore()
	sus := &recordingSuspender{err: errors.New("suspension store unavailable")}
	h := NewWebhookHandler(fakeProvider{ev: Event{Status: billing.StatusSuspended, CustomerRef: "cus_alice"}},
		fakeCustomers{"cus_alice": "org-alice"}, nil, status, nil, nil).WithSuspender(sus)

	w := post(h)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 so the provider retries", w.Code)
	}
	got, _ := status.Status(ctx, "org-alice")
	if got == billing.StatusSuspended {
		t.Error("billing status flipped to suspended although the kill-switch write failed; the retry would then be transition-gated away")
	}
}

// TestWebhookNonSuspendedStatusSkipsSuspender asserts a non-suspended status
// (past_due, active) never touches the suspender: dunning grace and recovery
// are status-only transitions.
func TestWebhookNonSuspendedStatusSkipsSuspender(t *testing.T) {
	status := billing.NewMemStatusStore()
	sus := &recordingSuspender{}
	h := NewWebhookHandler(fakeProvider{ev: Event{Status: billing.StatusPastDue, CustomerRef: "cus_alice"}},
		fakeCustomers{"cus_alice": "org-alice"}, nil, status, nil, nil).WithSuspender(sus)
	if w := post(h); w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if len(sus.calls) != 0 {
		t.Errorf("suspender fired for a past_due event: %+v", sus.calls)
	}
}
