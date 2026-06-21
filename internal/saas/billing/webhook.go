package billing

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"mitos.run/mitos/internal/apierr"
)

// WebhookEventType is the subset of Stripe webhook event types this slice acts
// on. Stripe sends many more; the handler ignores the rest. The strings match
// Stripe's event type names so the real adapter can map directly.
type WebhookEventType string

const (
	// EventTypePaymentSucceeded maps to Stripe invoice.payment_succeeded: the org's
	// charge cleared. Drives the dunning machine's payment-succeeded recovery.
	EventTypePaymentSucceeded WebhookEventType = "invoice.payment_succeeded"
	// EventTypePaymentFailed maps to Stripe invoice.payment_failed: a charge
	// failed. Drives the dunning machine into past_due.
	EventTypePaymentFailed WebhookEventType = "invoice.payment_failed"
)

// WebhookEvent is the verified, parsed Stripe webhook the handler acts on. It
// carries the org id (resolved from the Stripe customer id by the real adapter;
// the fake verifier puts it straight in) and the event type. It carries NO
// secret: not the signing secret, not a card detail.
type WebhookEvent struct {
	OrgID string           `json:"org_id"`
	Type  WebhookEventType `json:"type"`
}

// FakeVerifier is the test SignatureVerifier: it parses the body as a
// WebhookEvent and trusts it (the real signature check needs the signing secret,
// which is not available in this slice). The REAL verifier is a documented
// follow-up behind the SignatureVerifier interface that constant-time checks the
// Stripe-Signature header with the endpoint signing secret. See
// docs/saas/pricing.md, "Webhook signature seam".
type FakeVerifier struct{}

// Verify parses the body as a WebhookEvent without checking a signature. It is
// for tests and local development ONLY; production wires the real verifier.
func (FakeVerifier) Verify(payload []byte, _ string) (WebhookEvent, error) {
	var ev WebhookEvent
	if err := json.Unmarshal(payload, &ev); err != nil {
		return WebhookEvent{}, fmt.Errorf("parse webhook body: %w", err)
	}
	if ev.OrgID == "" {
		return WebhookEvent{}, fmt.Errorf("webhook event has no org")
	}
	return ev, nil
}

// WebhookHandler verifies and dispatches Stripe webhooks: it verifies the
// signature (through the SignatureVerifier seam), maps the event to a dunning
// event, and runs the dunning state machine, which updates the org's billing
// status and (on a transition into suspended) drives the #213 kill-switch. The
// handler NEVER logs the raw body, the signature header, or the signing secret.
type WebhookHandler struct {
	svc      *Service
	verifier SignatureVerifier
}

// NewWebhookHandler builds a webhook handler over a billing service and a
// signature verifier. Production passes the real verifier (with the signing
// secret); tests pass FakeVerifier.
func NewWebhookHandler(svc *Service, verifier SignatureVerifier) *WebhookHandler {
	return &WebhookHandler{svc: svc, verifier: verifier}
}

// Handle verifies and applies one webhook event, returning the org's new
// billing status. It is the seam ServeHTTP wraps; calling it directly is how the
// unit suite drives the handler with a fake-verified event.
func (h *WebhookHandler) Handle(ctx context.Context, payload []byte, signatureHeader string) (BillingStatus, error) {
	ev, err := h.verifier.Verify(payload, signatureHeader)
	if err != nil {
		// The signature did not verify (or the body did not parse). Return the
		// error WITHOUT echoing the payload or the header, so no secret leaks.
		return "", fmt.Errorf("webhook verify: %w", err)
	}
	dev, ok := dunningEventFor(ev.Type)
	if !ok {
		// An event type we do not act on: read the current status and return it
		// unchanged. This is not an error; Stripe sends many event types.
		return h.svc.status.Status(ctx, ev.OrgID)
	}
	return h.svc.applyDunning(ctx, ev.OrgID, dev)
}

// dunningEventFor maps a Stripe webhook event type to the dunning event it
// drives, or false if this slice does not act on it.
func dunningEventFor(t WebhookEventType) (DunningEvent, bool) {
	switch t {
	case EventTypePaymentSucceeded:
		return EventPaymentSucceeded, true
	case EventTypePaymentFailed:
		return EventPaymentFailed, true
	default:
		return "", false
	}
}

// ServeHTTP is the HTTP entry point for Stripe webhooks. It reads the raw body
// and the Stripe-Signature header, verifies and applies the event, and replies
// 200 on success so Stripe does not retry. On a verification failure it replies
// with an apierr envelope WITHOUT echoing the body or the signature.
func (h *WebhookHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		apierr.Encode(w, apierr.Get(apierr.CodeNotFound).
			WithCause("the webhook endpoint accepts POST only"))
		return
	}
	defer func() { _ = r.Body.Close() }()
	payload, err := readAll(r)
	if err != nil {
		apierr.Encode(w, apierr.Get(apierr.CodeInvalidJSON).
			WithCause("the webhook body could not be read"))
		return
	}
	sig := r.Header.Get("Stripe-Signature")
	if _, err := h.Handle(r.Context(), payload, sig); err != nil {
		// Do NOT include err's detail in the public cause: it could reference the
		// payload. A fixed, non-secret remediation string only.
		apierr.Encode(w, apierr.Get(apierr.CodeUnauthorized).
			WithCause("the webhook signature did not verify"))
		return
	}
	w.WriteHeader(http.StatusOK)
}

// readAll reads the entire request body with a sane bound so a webhook cannot
// stream an unbounded body. 1 MiB is far larger than any Stripe event.
func readAll(r *http.Request) ([]byte, error) {
	const maxBody = 1 << 20
	return io.ReadAll(io.LimitReader(r.Body, maxBody))
}
