// Package stripe is the Stripe implementation of billingprovider.Provider. It
// verifies the Stripe-Signature header (HMAC-SHA256 over "t.payload", with a
// timestamp tolerance for replay protection) and maps Stripe event types onto
// the neutral billingprovider.Event. The dunning/status core stays
// provider-neutral; swapping to a Merchant of Record means adding a sibling
// package, not touching the core.
package stripe

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"net/http"

	"mitos.run/mitos/internal/saas/billing"
	"mitos.run/mitos/internal/saas/billingprovider"
)

// Config wires the Stripe provider.
type Config struct {
	SigningSecret string                                                        // whsec_... endpoint signing secret
	Now           func() time.Time                                              // clock (testable)
	Tolerance     time.Duration                                                 // max age of a signed event; 0 disables the check
	Portal        func(ctx context.Context, customerRef string) (string, error) // creates a Customer Portal URL (live Stripe API)
}

// Provider implements billingprovider.Provider for Stripe.
type Provider struct {
	secret    string
	now       func() time.Time
	tolerance time.Duration
	portal    func(ctx context.Context, customerRef string) (string, error)
}

// New builds the Stripe provider.
func New(cfg Config) *Provider {
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &Provider{secret: cfg.SigningSecret, now: now, tolerance: cfg.Tolerance, portal: cfg.Portal}
}

func (p *Provider) Name() string { return "stripe" }

// VerifyWebhook authenticates the Stripe-Signature header against body and maps
// the event type to a neutral status.
func (p *Provider) VerifyWebhook(r *http.Request, body []byte) (billingprovider.Event, error) {
	if err := p.verifySignature(r.Header.Get("Stripe-Signature"), body); err != nil {
		return billingprovider.Event{}, err
	}
	var ev struct {
		Type string `json:"type"`
		Data struct {
			Object struct {
				Customer string `json:"customer"`
			} `json:"object"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &ev); err != nil {
		return billingprovider.Event{}, fmt.Errorf("stripe: malformed event: %w", err)
	}
	return billingprovider.Event{Status: statusFor(ev.Type), CustomerRef: ev.Data.Object.Customer}, nil
}

// statusFor maps Stripe event types to the neutral billing status. Unmapped
// types return "" (acknowledged but ignored).
func statusFor(eventType string) billing.BillingStatus {
	switch eventType {
	case "invoice.payment_failed":
		return billing.StatusPastDue
	case "invoice.payment_succeeded", "customer.subscription.updated":
		return billing.StatusActive
	case "customer.subscription.deleted":
		return billing.StatusSuspended
	default:
		return ""
	}
}

// verifySignature implements Stripe's scheme: signed_payload = "t.body",
// expected = hex(HMAC_SHA256(secret, signed_payload)), compared in constant time
// to a v1 signature, with an optional timestamp tolerance.
func (p *Provider) verifySignature(header string, body []byte) error {
	if p.secret == "" {
		return errors.New("stripe: no signing secret configured")
	}
	var ts string
	var v1s []string
	for _, part := range strings.Split(header, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		switch k {
		case "t":
			ts = v
		case "v1":
			v1s = append(v1s, v)
		}
	}
	if ts == "" || len(v1s) == 0 {
		return errors.New("stripe: malformed Stripe-Signature header")
	}
	if p.tolerance > 0 {
		sec, err := strconv.ParseInt(ts, 10, 64)
		if err != nil {
			return errors.New("stripe: bad signature timestamp")
		}
		if d := p.now().Sub(time.Unix(sec, 0)); d > p.tolerance || d < -p.tolerance {
			return errors.New("stripe: signature timestamp outside tolerance")
		}
	}
	mac := hmac.New(sha256.New, []byte(p.secret))
	mac.Write([]byte(ts))
	mac.Write([]byte("."))
	mac.Write(body)
	expected := mac.Sum(nil)
	for _, v1 := range v1s {
		got, err := hex.DecodeString(v1)
		if err != nil {
			continue
		}
		if subtle.ConstantTimeCompare(got, expected) == 1 {
			return nil
		}
	}
	return errors.New("stripe: signature mismatch")
}

// PortalURL returns a Stripe Customer Portal URL for the customer. The live
// Stripe API call is injected via Config.Portal; without it, the console hides
// the manage-subscription affordance.
func (p *Provider) PortalURL(ctx context.Context, customerRef string) (string, error) {
	if p.portal == nil {
		return "", errors.New("stripe: customer portal not configured")
	}
	return p.portal(ctx, customerRef)
}
