// Package billingprovider abstracts the payment backend behind a provider seam,
// the same way console.SecretStore abstracts the secret backend. Stripe is one
// provider; a Merchant of Record (Polar, Paddle, Lemon Squeezy) is another —
// and an MoR is the likely end state, since it becomes the legal seller and
// handles global sales-tax/VAT so mitos does not have to. The console reads
// billing through the provider-neutral BillingReader seam and never names a
// provider; only the webhook (signature scheme + event names) and the
// portal/checkout link are provider-specific, and both live behind this seam.
package billingprovider

import (
	"context"
	"io"
	"net/http"

	"mitos.run/mitos/internal/saas/billing"
)

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
// so the provider stops retrying.
type WebhookHandler struct {
	provider  Provider
	customers CustomerResolver
	status    billing.StatusStore
}

// NewWebhookHandler builds the webhook endpoint for a provider.
func NewWebhookHandler(p Provider, customers CustomerResolver, status billing.StatusStore) *WebhookHandler {
	return &WebhookHandler{provider: p, customers: customers, status: status}
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
