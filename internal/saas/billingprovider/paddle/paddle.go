// Package paddle is the Paddle Billing implementation of
// billingprovider.Provider. Paddle is a Merchant of Record: it is the legal
// seller and handles global sales-tax/VAT, so Mitos does not. The provider
// verifies the Paddle-Signature header (HMAC-SHA256 over "ts:body", with a
// timestamp tolerance for replay protection) and maps Paddle Billing event types
// onto the neutral billingprovider.Event. The dunning/status core stays
// provider-neutral; this package is a sibling to the Stripe provider, selected at
// wiring time.
//
// The Paddle Billing API is reached with net/http + encoding/json (no
// third-party SDK), a bearer API key, and a configurable base URL so the sandbox
// (https://sandbox-api.paddle.com) and live (https://api.paddle.com) hosts and a
// test httptest.Server are interchangeable.
//
// Security: the API key and the webhook signing secret are NEVER logged, NEVER
// placed in error messages, and NEVER returned to a caller. They live only in the
// Provider struct and the request headers.
package paddle

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"mitos.run/mitos/internal/saas/billing"
	"mitos.run/mitos/internal/saas/billingprovider"
)

// LiveBaseURL is the Paddle Billing live API host.
const LiveBaseURL = "https://api.paddle.com"

// SandboxBaseURL is the Paddle Billing sandbox API host.
const SandboxBaseURL = "https://sandbox-api.paddle.com"

// defaultTolerance is the replay window applied when Config.Tolerance is zero.
const defaultTolerance = 5 * time.Minute

// Config wires the Paddle provider. APIKey and WebhookSecret are SECRETS.
type Config struct {
	// APIKey is the Paddle Billing API key (bearer token). Empty disables the
	// portal API call; the webhook still verifies on WebhookSecret alone.
	APIKey string
	// WebhookSecret is the endpoint's signing secret (the per-notification
	// destination secret). Empty makes VerifyWebhook fail closed.
	WebhookSecret string
	// BaseURL is the Paddle API host; defaults to LiveBaseURL. Point it at
	// SandboxBaseURL for the sandbox, or at an httptest.Server in tests.
	BaseURL string
	// HTTPClient is the client used for API calls; defaults to a 15s client.
	HTTPClient *http.Client
	// Now is the clock used for timestamp tolerance (testable); defaults to
	// time.Now.
	Now func() time.Time
	// Tolerance is the maximum age of a signed event; defaults to 5 minutes. A
	// negative value disables the timestamp check.
	Tolerance time.Duration
}

// Provider implements billingprovider.Provider for Paddle Billing.
type Provider struct {
	apiKey    string
	secret    string
	baseURL   string
	http      *http.Client
	now       func() time.Time
	tolerance time.Duration
}

// New builds the Paddle provider from cfg, applying the documented defaults.
func New(cfg Config) *Provider {
	base := cfg.BaseURL
	if base == "" {
		base = LiveBaseURL
	}
	base = strings.TrimRight(base, "/")
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	tol := cfg.Tolerance
	if tol == 0 {
		tol = defaultTolerance
	}
	return &Provider{
		apiKey:    cfg.APIKey,
		secret:    cfg.WebhookSecret,
		baseURL:   base,
		http:      client,
		now:       now,
		tolerance: tol,
	}
}

// Name identifies the provider in logs and capabilities.
func (p *Provider) Name() string { return "paddle" }

// VerifyWebhook authenticates the Paddle-Signature header against body and maps
// the Paddle event type to a neutral billing status. A verification error means
// the request is forged, replayed, or malformed and the caller MUST refuse it.
func (p *Provider) VerifyWebhook(r *http.Request, body []byte) (billingprovider.Event, error) {
	if err := p.verifySignature(r.Header.Get("Paddle-Signature"), body); err != nil {
		return billingprovider.Event{}, err
	}
	// Paddle Billing webhook envelope. The customer id lives at data.customer_id
	// for subscription and transaction events; subscription.canceled etc. all
	// carry it. We read only what the neutral event needs.
	var ev struct {
		EventType string `json:"event_type"`
		Data      struct {
			CustomerID string `json:"customer_id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &ev); err != nil {
		return billingprovider.Event{}, fmt.Errorf("paddle: malformed event: %w", err)
	}
	return billingprovider.Event{
		Status:      statusFor(ev.EventType),
		CustomerRef: ev.Data.CustomerID,
	}, nil
}

// statusFor maps Paddle Billing event types to the neutral billing status,
// matching the Stripe provider's semantics: a healthy subscription/transaction is
// active, a payment failure or past_due is the dunning trigger, and a cancellation
// suspends. Unmapped types return "" (acknowledged but ignored).
func statusFor(eventType string) billing.BillingStatus {
	switch eventType {
	case "subscription.created",
		"subscription.activated",
		"subscription.resumed",
		"subscription.updated",
		"transaction.completed",
		"transaction.paid":
		return billing.StatusActive
	case "subscription.past_due",
		"transaction.payment_failed":
		return billing.StatusPastDue
	case "subscription.canceled",
		"subscription.paused":
		return billing.StatusSuspended
	default:
		return ""
	}
}

// verifySignature implements Paddle Billing's scheme: the Paddle-Signature header
// is "ts=<unix>;h1=<hex hmac>", where h1 = hex(HMAC_SHA256(secret, "<ts>:<body>")).
// The comparison is constant time and an optional timestamp tolerance bounds
// replay. It fails CLOSED: an empty secret, a missing or malformed header, a bad
// timestamp, or a mismatched MAC all return an error.
func (p *Provider) verifySignature(header string, body []byte) error {
	if p.secret == "" {
		return errors.New("paddle: no webhook signing secret configured")
	}
	var ts, h1 string
	for _, part := range strings.Split(header, ";") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		switch k {
		case "ts":
			ts = v
		case "h1":
			h1 = v
		}
	}
	if ts == "" || h1 == "" {
		return errors.New("paddle: malformed Paddle-Signature header")
	}
	if p.tolerance >= 0 {
		sec, err := strconv.ParseInt(ts, 10, 64)
		if err != nil {
			return errors.New("paddle: bad signature timestamp")
		}
		if d := p.now().Sub(time.Unix(sec, 0)); d > p.tolerance || d < -p.tolerance {
			return errors.New("paddle: signature timestamp outside tolerance")
		}
	}
	want, err := hex.DecodeString(h1)
	if err != nil {
		return errors.New("paddle: signature is not valid hex")
	}
	mac := hmac.New(sha256.New, []byte(p.secret))
	mac.Write([]byte(ts))
	mac.Write([]byte(":"))
	mac.Write(body)
	expected := mac.Sum(nil)
	if subtle.ConstantTimeCompare(want, expected) != 1 {
		return errors.New("paddle: signature mismatch")
	}
	return nil
}

// PortalURL returns a Paddle-hosted customer portal URL ("manage subscription")
// for the customer, via Paddle Billing's POST
// /customers/{id}/portal-sessions endpoint. The console deep-links to it rather
// than rebuilding payment UI. The API key is sent as a bearer token and is never
// surfaced in the returned error.
func (p *Provider) PortalURL(ctx context.Context, customerRef string) (string, error) {
	if p.apiKey == "" {
		return "", errors.New("paddle: customer portal not configured (no API key)")
	}
	if customerRef == "" {
		return "", errors.New("paddle: empty customer reference")
	}
	url := p.baseURL + "/customers/" + customerRef + "/portal-sessions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader([]byte("{}")))
	if err != nil {
		return "", fmt.Errorf("paddle: build portal request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+p.apiKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("paddle: portal request: %w", err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("paddle: read portal response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("paddle: portal API returned status %d", resp.StatusCode)
	}
	// Paddle Billing portal-sessions response shape:
	// { "data": { "urls": { "general": { "overview": "https://..." } } } }
	var out struct {
		Data struct {
			URLs struct {
				General struct {
					Overview string `json:"overview"`
				} `json:"general"`
			} `json:"urls"`
		} `json:"data"`
	}
	if err := json.Unmarshal(respBody, &out); err != nil {
		return "", fmt.Errorf("paddle: malformed portal response: %w", err)
	}
	if out.Data.URLs.General.Overview == "" {
		return "", errors.New("paddle: portal response had no overview URL")
	}
	return out.Data.URLs.General.Overview, nil
}
