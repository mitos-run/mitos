package billing

import (
	"context"
	"errors"
	"sync"
)

// StripeClient is the seam over the Stripe API. The billing service is written
// ENTIRELY against this interface so the verifiable core (metered push, credit
// ledger, spend caps, dunning, webhooks) is unit-tested with FakeStripe and no
// network, no SDK, and no keys. The REAL adapter is a documented follow-up
// behind this exact interface: a small file in internal/saas/billing (for
// example stripe_sdk.go, build-tagged or guarded so the SDK is not a hard
// dependency of this slice) that wraps the Stripe Go SDK and is exercised by a
// maintainer with test-mode keys. See docs/saas/pricing.md, "Real Stripe
// adapter seam".
//
// Security: implementations MUST NOT log or surface the API key, the signing
// secret, or any payment method detail. Method arguments carry ids and counts
// only; the secret handle lives in the adapter's construction, not in calls.
type StripeClient interface {
	// EnsureProductAndPrice creates (or finds, idempotently) the Stripe product
	// and metered price for a billing dimension, returning the price id. The real
	// adapter looks the price up by a stable lookup key so repeated calls do not
	// create duplicates.
	EnsureProductAndPrice(ctx context.Context, unit MeterUnit) (priceID string, err error)

	// EnsureCustomer creates (or finds) the Stripe customer for an org, returning
	// the customer id. Idempotent on org id.
	EnsureCustomer(ctx context.Context, orgID string) (customerID string, err error)

	// ReportUsage reports a metered usage event to Stripe. idempotencyKey is the
	// (org, sandbox, window)+meter key: Stripe de-duplicates on the idempotency
	// key so a RETRIED push with the same key never double-reports. The real
	// adapter passes idempotencyKey as the Stripe Idempotency-Key request header.
	ReportUsage(ctx context.Context, ev UsageEvent) error

	// CreateSubscription starts a metered subscription for the org's customer over
	// the metered prices, returning the subscription id. Lifecycle (upgrade,
	// cancel) is a documented follow-up; this slice creates and reads status.
	CreateSubscription(ctx context.Context, customerID string, priceIDs []string) (subscriptionID string, err error)

	// CreateInvoice finalizes the period's metered usage into an invoice for the
	// customer, returning the invoice id. The fake records the call; the real
	// adapter calls Stripe invoicing.
	CreateInvoice(ctx context.Context, customerID string) (invoiceID string, err error)

	// HasPaymentMethod reports whether the customer has a usable payment method.
	// Dunning and spend-cap policy read this; the fake lets a test toggle it.
	HasPaymentMethod(ctx context.Context, customerID string) (bool, error)
}

// UsageEvent is one metered usage push: the meter, the quantity in the meter's
// natural unit, the Stripe customer, and the idempotency key that makes the push
// safe to retry. Timestamp is the window start (the billable instant). It
// carries NO secret.
type UsageEvent struct {
	CustomerID     string
	Unit           MeterUnit
	Quantity       float64
	IdempotencyKey string
}

// SignatureVerifier is the seam for Stripe webhook signature verification. The
// REAL verifier needs the endpoint signing secret and calls the Stripe library
// to constant-time check the Stripe-Signature header; that secret is NOT
// available in this slice, so verification is interfaced. The fake verifier in
// tests treats a payload as already-verified. See docs/saas/pricing.md.
//
// Security: an implementation MUST NOT log the signing secret or the raw
// signature header.
type SignatureVerifier interface {
	// Verify checks the raw webhook body against the Stripe-Signature header and
	// returns the typed event if valid, or an error if the signature does not
	// verify. The real implementation holds the signing secret; the fake parses
	// the body and trusts it.
	Verify(payload []byte, signatureHeader string) (WebhookEvent, error)
}

// FakeStripe is the in-memory StripeClient used by the unit suite. It records
// every reported usage event keyed by idempotency key so a retried push is a
// no-op overwrite (proving idempotency), tracks ensured products/customers, and
// lets a test toggle payment-method presence. It is safe for concurrent use.
// It NEVER touches the network and holds NO secret.
type FakeStripe struct {
	mu sync.Mutex

	// reported maps idempotency key -> the event last reported under it. Because a
	// retried push uses the SAME key, re-reporting overwrites rather than appends,
	// so len(reported) is the count of DISTINCT billable usage events. This is the
	// property the idempotent-push test asserts.
	reported map[string]UsageEvent

	prices     map[MeterUnit]string
	customers  map[string]string
	subs       map[string]string
	invoices   []string
	hasPayment map[string]bool

	// failReportFor, when set to an idempotency key, makes ReportUsage return an
	// error for that key once, so a test can drive the retry path.
	failReportFor map[string]int
}

// NewFakeStripe returns an empty fake client.
func NewFakeStripe() *FakeStripe {
	return &FakeStripe{
		reported:      map[string]UsageEvent{},
		prices:        map[MeterUnit]string{},
		customers:     map[string]string{},
		subs:          map[string]string{},
		hasPayment:    map[string]bool{},
		failReportFor: map[string]int{},
	}
}

// ErrFakeReportFailed is returned by ReportUsage when a test has armed a
// transient failure for an idempotency key (to drive the retry path).
var ErrFakeReportFailed = errors.New("billing: fake stripe report failed (test-armed)")

// ArmReportFailure makes the next n ReportUsage calls for key fail with
// ErrFakeReportFailed, so a test can prove a retried push is idempotent.
func (f *FakeStripe) ArmReportFailure(key string, n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failReportFor[key] = n
}

// SetPaymentMethod toggles whether a customer has a usable payment method, so a
// test can drive the dunning and spend-cap payment paths.
func (f *FakeStripe) SetPaymentMethod(customerID string, ok bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hasPayment[customerID] = ok
}

// ReportedCount returns the number of DISTINCT usage events (by idempotency
// key) reported to the fake. A retried push does not increase it.
func (f *FakeStripe) ReportedCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.reported)
}

// Reported returns the event recorded under an idempotency key and whether one
// was reported.
func (f *FakeStripe) Reported(key string) (UsageEvent, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ev, ok := f.reported[key]
	return ev, ok
}

func (f *FakeStripe) EnsureProductAndPrice(_ context.Context, unit MeterUnit) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if id, ok := f.prices[unit]; ok {
		return id, nil
	}
	id := "price_" + string(unit)
	f.prices[unit] = id
	return id, nil
}

func (f *FakeStripe) EnsureCustomer(_ context.Context, orgID string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if id, ok := f.customers[orgID]; ok {
		return id, nil
	}
	id := "cus_" + orgID
	f.customers[orgID] = id
	return id, nil
}

func (f *FakeStripe) ReportUsage(_ context.Context, ev UsageEvent) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if n := f.failReportFor[ev.IdempotencyKey]; n > 0 {
		f.failReportFor[ev.IdempotencyKey] = n - 1
		return ErrFakeReportFailed
	}
	// Overwrite by idempotency key: a retried push with the same key is a no-op
	// for the distinct-event count, which is exactly Stripe's idempotency contract.
	f.reported[ev.IdempotencyKey] = ev
	return nil
}

func (f *FakeStripe) CreateSubscription(_ context.Context, customerID string, _ []string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if id, ok := f.subs[customerID]; ok {
		return id, nil
	}
	id := "sub_" + customerID
	f.subs[customerID] = id
	return id, nil
}

func (f *FakeStripe) CreateInvoice(_ context.Context, customerID string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := "in_" + customerID
	f.invoices = append(f.invoices, id)
	return id, nil
}

func (f *FakeStripe) HasPaymentMethod(_ context.Context, customerID string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hasPayment[customerID], nil
}
