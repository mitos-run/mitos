package billing

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// failingStatusStore makes the post-verify processing path fail, modelling a
// transient backend outage while the webhook signature itself is valid.
type failingStatusStore struct{}

func (failingStatusStore) Status(context.Context, string) (BillingStatus, error) {
	return "", errors.New("status backend down")
}
func (failingStatusStore) SetStatus(context.Context, string, BillingStatus) error { return nil }

// TestWebhookInternalErrorReturns500NotUnauthorized proves a post-verify failure
// (the signature verified, but applying the event hit a backend error) returns
// 500, not 401. 401 would both mask an internal failure as a signature problem
// (misrouting on-call to rotate the webhook secret) and mislabel a retryable
// server fault as a client auth failure.
func TestWebhookInternalErrorReturns500NotUnauthorized(t *testing.T) {
	svc := NewService(Config{Stripe: NewFakeStripe(), Status: failingStatusStore{}, Now: fixedNow})
	h := NewWebhookHandler(svc, FakeVerifier{})

	body, _ := json.Marshal(WebhookEvent{OrgID: "org1", Type: EventTypePaymentFailed})
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("internal processing error must return 500, got %d", rec.Code)
	}
}

// TestWebhookSignatureFailureReturns401 locks in that a genuine verify failure
// still maps to 401 after the internal-vs-signature split.
func TestWebhookSignatureFailureReturns401(t *testing.T) {
	svc := NewService(Config{Stripe: NewFakeStripe(), Now: fixedNow})
	h := NewWebhookHandler(svc, FakeVerifier{})

	// FakeVerifier rejects a body with no org as a verify failure.
	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(`{"type":"invoice.payment_failed"}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("signature/verify failure must return 401, got %d", rec.Code)
	}
}

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
