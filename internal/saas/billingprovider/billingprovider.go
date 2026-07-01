// Package billingprovider abstracts the payment backend behind a provider seam,
// the same way console.SecretStore abstracts the secret backend. Stripe is one
// provider; a Merchant of Record (Polar, Paddle, Lemon Squeezy) is another —
// and an MoR is the likely end state, since it becomes the legal seller and
// handles global sales-tax/VAT so Mitos does not have to. The console reads
// billing through the provider-neutral BillingReader seam and never names a
// provider; only the webhook (signature scheme + event names) and the
// portal/checkout link are provider-specific, and both live behind this seam.
package billingprovider

import (
	"context"
	"errors"
	"io"
	"net/http"
	"time"

	"mitos.run/mitos/internal/saas/billing"
)

// TopUpCredit is a cleared prepaid credit purchase carried by an Event. OrgID
// comes from the signature-verified custom_data (NOT the customer map) so a
// first-time, not-yet-mapped customer is still credited. Ref is the provider
// transaction id (NOT a secret): the idempotency key.
type TopUpCredit struct {
	OrgID       string
	AmountCents int64
	Ref         string
}

// Event is a provider-NEUTRAL billing event. Each Provider maps its own webhook
// payload onto this shape so the dunning/status core never depends on a
// provider. An empty Status means "no status change" (an event we acknowledge
// but ignore).
type Event struct {
	// Status is the billing status this event implies, or "" to ignore.
	Status billing.BillingStatus
	// CustomerRef is the provider's customer identifier, resolved to an org via
	// the CustomerResolver.
	CustomerRef string
	// TopUp carries the inputs for a prepaid credit purchase, or nil when the
	// event is not a cleared top-up. OrgID comes from the signature-verified
	// custom_data so a first-time customer is still credited.
	TopUp *TopUpCredit
}

// Provider is the payment-backend seam. VerifyWebhook authenticates the request
// (signature/timestamp) and returns a normalized Event; an error means the
// request is forged, replayed, or malformed and must be refused.
type Provider interface {
	Name() string
	VerifyWebhook(r *http.Request, body []byte) (Event, error)
	// PortalURL returns a provider-hosted "manage subscription" URL for a
	// customer (Stripe Customer Portal, or the MoR's equivalent). The console
	// deep-links to it rather than rebuilding payment UI.
	PortalURL(ctx context.Context, customerRef string) (string, error)
}

// TopUp carries the inputs for a prepaid credit checkout. AmountCents is the
// integer cent amount (e.g. 5000 = EUR 50.00); it is sent to the provider as a
// string. CustomerRef is omitted from the provider request when empty.
type TopUp struct {
	// CustomerRef is the provider's customer identifier (e.g. Paddle ctm_…).
	// Omit for guest checkouts.
	CustomerRef string
	// OrgID is the Mitos org identifier recorded in the transaction custom data
	// for reconciliation.
	OrgID string
	// AmountCents is the top-up amount in integer cents (e.g. 5000 = EUR 50.00).
	AmountCents int64
	// ProductID is the provider product that represents a credit top-up.
	ProductID string
	// Currency is the ISO 4217 currency code (e.g. "EUR").
	Currency string
}

// CustomerResolver maps a provider customer id to the owning org. The mapping is
// recorded when the org first subscribes.
type CustomerResolver interface {
	OrgForCustomer(ctx context.Context, customerRef string) (string, bool)
}

const maxWebhookBytes = 1 << 20 // 1 MiB

// WebhookHandler is the provider-NEUTRAL webhook endpoint: it verifies the
// request through the Provider, resolves the customer to an org, and applies the
// normalized status to the StatusStore. Forged/replayed requests are refused
// (400); events for unknown customers or with no status are acknowledged (2xx)
// so the provider stops retrying. When a ledger is provided and the event
// carries a TopUpCredit, the credit is applied before the status block.
type WebhookHandler struct {
	provider  Provider
	customers CustomerResolver
	status    billing.StatusStore
	ledger    billing.CreditLedger
	now       func() time.Time
}

// NewWebhookHandler builds the webhook endpoint for a provider. A nil ledger
// disables top-up crediting (community edition). A nil now defaults to
// time.Now.
func NewWebhookHandler(p Provider, customers CustomerResolver, status billing.StatusStore, ledger billing.CreditLedger, now func() time.Time) *WebhookHandler {
	if now == nil {
		now = time.Now
	}
	return &WebhookHandler{provider: p, customers: customers, status: status, ledger: ledger, now: now}
}

func (h *WebhookHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxWebhookBytes))
	if err != nil {
		http.Error(w, "read error", http.StatusBadRequest)
		return
	}
	ev, err := h.provider.VerifyWebhook(r, body)
	if err != nil {
		// Refuse forged/replayed/malformed events. Never mutate billing state.
		http.Error(w, "webhook verification failed", http.StatusBadRequest)
		return
	}
	// Credit the top-up BEFORE the status/customer-resolve block: the org id
	// comes from the signature-verified custom_data and must not depend on the
	// customer map. A duplicate-entry error means the webhook was redelivered
	// and the credit already exists; treat it as success so the provider stops
	// retrying.
	if ev.TopUp != nil && h.ledger != nil && ev.TopUp.OrgID != "" && ev.TopUp.AmountCents > 0 {
		if err := billing.TopUp(r.Context(), h.ledger, ev.TopUp.OrgID, billing.Money(ev.TopUp.AmountCents), ev.TopUp.Ref, h.now()); err != nil {
			if !errors.Is(err, billing.ErrDuplicateEntry) {
				http.Error(w, "credit top-up failed", http.StatusInternalServerError)
				return
			}
		}
	}
	if ev.Status == "" {
		w.WriteHeader(http.StatusOK) // acknowledged, nothing to apply
		return
	}
	orgID, ok := h.customers.OrgForCustomer(r.Context(), ev.CustomerRef)
	if !ok {
		w.WriteHeader(http.StatusOK) // unknown customer: ack so retries stop
		return
	}
	if err := h.status.SetStatus(r.Context(), orgID, ev.Status); err != nil {
		http.Error(w, "status update failed", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}
