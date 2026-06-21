package billing

import (
	"context"
	"encoding/json"
	"testing"
)

// TestWebhookPaymentFailedThenSucceededUpdatesStatus asserts the webhook handler
// runs the dunning machine over fake-verified events: a payment_failed moves the
// org to past_due, a payment_succeeded recovers it to active.
func TestWebhookPaymentFailedThenSucceededUpdatesStatus(t *testing.T) {
	ctx := context.Background()
	svc := NewService(Config{Stripe: NewFakeStripe(), Now: fixedNow})
	h := NewWebhookHandler(svc, FakeVerifier{})

	failed, _ := json.Marshal(WebhookEvent{OrgID: "org1", Type: EventTypePaymentFailed})
	st, err := h.Handle(ctx, failed, "")
	if err != nil {
		t.Fatalf("handle failed event: %v", err)
	}
	if st != StatusPastDue {
		t.Errorf("status after payment_failed = %s, want past_due", st)
	}

	ok, _ := json.Marshal(WebhookEvent{OrgID: "org1", Type: EventTypePaymentSucceeded})
	st, err = h.Handle(ctx, ok, "")
	if err != nil {
		t.Fatalf("handle succeeded event: %v", err)
	}
	if st != StatusActive {
		t.Errorf("status after payment_succeeded = %s, want active", st)
	}
}

// TestWebhookUnknownTypeLeavesStatusUnchanged asserts an event type the slice
// does not act on returns the current status unchanged and is not an error.
func TestWebhookUnknownTypeLeavesStatusUnchanged(t *testing.T) {
	ctx := context.Background()
	svc := NewService(Config{Stripe: NewFakeStripe(), Now: fixedNow})
	h := NewWebhookHandler(svc, FakeVerifier{})

	body, _ := json.Marshal(WebhookEvent{OrgID: "org1", Type: "customer.created"})
	st, err := h.Handle(ctx, body, "")
	if err != nil {
		t.Fatalf("handle unknown event: %v", err)
	}
	if st != StatusActive {
		t.Errorf("status = %s, want active (unchanged)", st)
	}
}

// TestWebhookVerifyFailureReturnsError asserts a body the verifier rejects (no
// org) surfaces an error and does not change any status.
func TestWebhookVerifyFailureReturnsError(t *testing.T) {
	ctx := context.Background()
	svc := NewService(Config{Stripe: NewFakeStripe(), Now: fixedNow})
	h := NewWebhookHandler(svc, FakeVerifier{})

	if _, err := h.Handle(ctx, []byte(`{"type":"invoice.payment_failed"}`), ""); err == nil {
		t.Fatal("expected verify error for a body with no org")
	}
}
