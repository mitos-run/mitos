// Package billingprovider abstracts the payment backend behind a provider seam,
// the same way console.SecretStore abstracts the secret backend. Stripe is one
// provider; a Merchant of Record (Polar, Paddle, Lemon Squeezy) is another,
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
	// OrgID is the Mitos org named by the event's signature-verified custom
	// data (the org_id we embedded at checkout), or "" when the event carries
	// none. When both OrgID and CustomerRef are present the webhook handler
	// records the org <-> customer link, so the very first event for a new
	// customer establishes the mapping later custom-data-less events
	// (subscription.canceled etc.) resolve through.
	OrgID string
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

// Checkout is the result of creating a provider-hosted checkout: the URL to
// send the buyer to, and the provider customer id the transaction was created
// for. CustomerRef is "" when the provider's response names no customer (a
// first-time buyer: the customer is created only when the hosted checkout
// collects an email, and the webhook-time link covers it).
type Checkout struct {
	URL         string
	CustomerRef string
}

// CustomerResolver maps a provider customer id to the owning org. The mapping is
// recorded when the org first subscribes. An unknown customer is ("", false,
// nil); a store failure is an error the webhook must answer 5xx with, so the
// provider retries instead of the event being dropped as unknown.
type CustomerResolver interface {
	OrgForCustomer(ctx context.Context, customerRef string) (string, bool, error)
}

// CustomerLinker is the write half of the org <-> customer map: Link records
// the association (idempotent, last-write-wins). The webhook handler calls it
// when an event's signature-verified custom_data names the org, so the map is
// populated without any out-of-band step (issue #618).
type CustomerLinker interface {
	Link(ctx context.Context, orgID, customerRef string) error
}

const maxWebhookBytes = 1 << 20 // 1 MiB

// WebhookHandler is the provider-NEUTRAL webhook endpoint: it verifies the
// request through the Provider, records the org <-> customer link when the
// event names both sides, resolves the customer to an org, and applies the
// normalized status to the StatusStore. Forged/replayed requests are refused
// (400); events for unknown customers or with no status are acknowledged (2xx)
// so the provider stops retrying. When a ledger is provided and the event
// carries a TopUpCredit, the credit is applied before the status block.
type WebhookHandler struct {
	provider  Provider
	customers CustomerResolver
	linker    CustomerLinker
	status    billing.StatusStore
	ledger    billing.CreditLedger
	suspender billing.Suspender
	// lifter is the recovery half of the kill-switch seam, set by WithSuspender
	// when the suspender also implements billing.SuspensionLifter (the
	// production quota.BillingSuspender does): a paid top-up lifts spend_cap,
	// an active-status transition lifts dunning. Reason-scoped; manual holds
	// survive.
	lifter billing.SuspensionLifter
	now    func() time.Time
	// metrics counts verify failures and 5xx handler errors for the #617
	// billing alerts. Nil (the default) disables all observation.
	metrics *WebhookMetrics
}

// NewWebhookHandler builds the webhook endpoint for a provider. A nil linker
// disables webhook-time customer linking; a nil ledger disables top-up
// crediting (community edition). A nil now defaults to time.Now.
func NewWebhookHandler(p Provider, customers CustomerResolver, linker CustomerLinker, status billing.StatusStore, ledger billing.CreditLedger, now func() time.Time) *WebhookHandler {
	if now == nil {
		now = time.Now
	}
	return &WebhookHandler{provider: p, customers: customers, linker: linker, status: status, ledger: ledger, now: now}
}

// WithSuspender wires the #213 kill-switch seam into the webhook (issue #615):
// an event whose normalized status is suspended (subscription canceled or
// paused, payment retries exhausted) then SUSPENDS the org through s on the
// transition INTO suspended, so the org fails closed at the gateway instead of
// only its billing status flipping. The suspend fires BEFORE the status is
// applied and a suspend failure is a 5xx (the provider retries); the
// transition gate keeps webhook redeliveries from churning the suspension
// record. Nil (the default) keeps the status-only behavior (community edition
// without a wired kill-switch). When s also implements
// billing.SuspensionLifter (the production quota.BillingSuspender does), the
// recovery half is wired too: a cleared paid top-up lifts a spend_cap
// suspension and an active-status transition lifts a dunning suspension, both
// reason-scoped; a manual-hold suspension survives every automated lift and
// clears only through the operator hook (see docs/saas/pricing.md).
func (h *WebhookHandler) WithSuspender(s billing.Suspender) *WebhookHandler {
	h.suspender = s
	if l, ok := s.(billing.SuspensionLifter); ok {
		h.lifter = l
	}
	return h
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
		h.metrics.observeVerifyFailure()
		http.Error(w, "webhook verification failed", http.StatusBadRequest)
		return
	}
	// Record the org <-> customer link FIRST, before any processing: the org id
	// comes from the signature-verified custom_data, so the very first event for
	// a new customer establishes the mapping every later custom-data-less event
	// (subscription.canceled etc.) resolves through, and this event's own status
	// block already sees it. Link is idempotent (last-write-wins), so a
	// redelivered event is harmless. A link-store FAILURE is a 5xx, never an
	// ack: the provider retries instead of the mapping being dropped silently
	// (the same posture as the customer-lookup failure below, issue #614).
	if h.linker != nil && ev.OrgID != "" && ev.CustomerRef != "" {
		if err := h.linker.Link(r.Context(), ev.OrgID, ev.CustomerRef); err != nil {
			h.metrics.observeHandlerError()
			http.Error(w, "customer link failed", http.StatusInternalServerError)
			return
		}
	}
	// Credit the top-up BEFORE the status/customer-resolve block: the org id
	// comes from the signature-verified custom_data and must not depend on the
	// customer map. A duplicate-entry error means the webhook was redelivered
	// and the credit already exists; treat it as success so the provider stops
	// retrying.
	if ev.TopUp != nil && h.ledger != nil && ev.TopUp.OrgID != "" && ev.TopUp.AmountCents > 0 {
		if err := billing.TopUp(r.Context(), h.ledger, ev.TopUp.OrgID, billing.Money(ev.TopUp.AmountCents), ev.TopUp.Ref, h.now()); err != nil {
			if !errors.Is(err, billing.ErrDuplicateEntry) {
				h.metrics.observeHandlerError()
				http.Error(w, "credit top-up failed", http.StatusInternalServerError)
				return
			}
		}
		// Recovery (issue #615): the org paid, so a spend_cap suspension lifts
		// (the spend window resets at this payment, see
		// billing.Service.EnforceSpendCapFromLedger, so the next drawdown cycle
		// does not re-suspend for pre-payment spend). Attempted on redeliveries
		// too: LiftReason is idempotent, and gating it on a fresh credit could
		// strand the lift if the first delivery failed after crediting. A lift
		// failure is a 5xx so the provider retries; a manual hold survives (the
		// lift reports false and the ack proceeds; a held org paying is fine).
		if h.lifter != nil {
			lifted, err := h.lifter.LiftReason(r.Context(), ev.TopUp.OrgID, "spend_cap")
			if err != nil {
				h.metrics.observeHandlerError()
				http.Error(w, "suspension lift failed", http.StatusInternalServerError)
				return
			}
			if lifted {
				// The spend-cap breach also set the billing status to suspended;
				// restore it so the console and the dunning machine agree with
				// the lifted kill-switch.
				cur, err := h.status.Status(r.Context(), ev.TopUp.OrgID)
				if err != nil {
					h.metrics.observeHandlerError()
					http.Error(w, "status read failed", http.StatusInternalServerError)
					return
				}
				if cur == billing.StatusSuspended {
					if err := h.status.SetStatus(r.Context(), ev.TopUp.OrgID, billing.StatusActive); err != nil {
						h.metrics.observeHandlerError()
						http.Error(w, "status update failed", http.StatusInternalServerError)
						return
					}
				}
			}
		}
	}
	if ev.Status == "" {
		w.WriteHeader(http.StatusOK) // acknowledged, nothing to apply
		return
	}
	orgID, ok, err := h.customers.OrgForCustomer(r.Context(), ev.CustomerRef)
	if err != nil {
		// A lookup FAILURE must not be acked as "unknown customer": a 5xx makes
		// the provider retry, so a transient store error (e.g. right after a
		// restart) cannot permanently drop a status sync. No detail is echoed.
		h.metrics.observeHandlerError()
		http.Error(w, "customer lookup failed", http.StatusInternalServerError)
		return
	}
	if !ok {
		w.WriteHeader(http.StatusOK) // unknown customer: ack so retries stop
		return
	}
	// Transition INTO suspended drives the kill-switch BEFORE the status is
	// applied: if the suspension write fails the whole event is a 5xx and the
	// provider retries with the status still unflipped, so the transition gate
	// cannot swallow the retry and the org can never read as suspended in
	// billing while the gateway still admits it. The gate (current status not
	// already suspended) keeps redeliveries from churning the suspension time.
	// The note is non-secret; no payment detail crosses this seam.
	// Recovery (issue #615): a payment-recovered subscription (normalized
	// status active) lifts a dunning suspension. Attempted regardless of the
	// stored status (LiftReason is an idempotent no-op when not suspended, and
	// gating on the status store could strand a lift across a store skew). A
	// lift failure is a 5xx BEFORE the status flips, so the provider retries
	// and the org can never read active in billing while the gateway still
	// blocks it. Manual holds survive (reason-scoped lift).
	if h.lifter != nil && ev.Status == billing.StatusActive {
		if _, err := h.lifter.LiftReason(r.Context(), orgID, "dunning"); err != nil {
			h.metrics.observeHandlerError()
			http.Error(w, "suspension lift failed", http.StatusInternalServerError)
			return
		}
	}
	if h.suspender != nil && ev.Status == billing.StatusSuspended {
		cur, err := h.status.Status(r.Context(), orgID)
		if err != nil {
			h.metrics.observeHandlerError()
			http.Error(w, "status read failed", http.StatusInternalServerError)
			return
		}
		if cur != billing.StatusSuspended {
			if err := h.suspender.Suspend(r.Context(), orgID, "dunning",
				"billing provider reported the subscription suspended (canceled, paused, or payment retries exhausted)", false); err != nil {
				h.metrics.observeHandlerError()
				http.Error(w, "suspend failed", http.StatusInternalServerError)
				return
			}
		}
	}
	if err := h.status.SetStatus(r.Context(), orgID, ev.Status); err != nil {
		h.metrics.observeHandlerError()
		http.Error(w, "status update failed", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}
