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
	// carry it. For credit top-up transactions we also read data.id (the
	// transaction id, used as the idempotency ref) and data.custom_data (the
	// org/amount we embedded at checkout).
	var ev struct {
		EventType string `json:"event_type"`
		Data      struct {
			ID         string `json:"id"`
			CustomerID string `json:"customer_id"`
			CustomData struct {
				Kind        string `json:"kind"`
				OrgID       string `json:"org_id"`
				AmountCents string `json:"amount_cents"`
			} `json:"custom_data"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &ev); err != nil {
		return billingprovider.Event{}, fmt.Errorf("paddle: malformed event: %w", err)
	}
	out := billingprovider.Event{
		Status:      statusFor(ev.EventType),
		CustomerRef: ev.Data.CustomerID,
		// The org id we embedded in custom_data at checkout, surfaced so the
		// webhook handler can record the org <-> customer link on the first
		// event for a new customer (issue #618). "" when absent.
		OrgID: ev.Data.CustomData.OrgID,
	}
	// A credit top-up rides on the same transaction.completed / transaction.paid
	// event types as a subscription payment. It is a one-off prepaid purchase and
	// carries NO subscription-state meaning, so it must never move the org's
	// billing status: otherwise a past_due or suspended org that buys credits would
	// silently clear its own dunning. Suppress the status for a credit_topup
	// transaction (the credit itself is applied via TopUp below), independent of
	// whether the amount parses. A plain (non-top-up) transaction keeps its status.
	isTopUpTxn := (ev.EventType == "transaction.completed" || ev.EventType == "transaction.paid") &&
		ev.Data.CustomData.Kind == "credit_topup"
	if isTopUpTxn {
		out.Status = ""
		// Populate the credit when org_id is present and amount_cents parses to a
		// positive value. A missing, zero, or malformed amount leaves TopUp nil; the
		// event is still acknowledged. We do NOT fail verification for a malformed
		// top-up field.
		if ev.Data.CustomData.OrgID != "" {
			cents, err := strconv.ParseInt(ev.Data.CustomData.AmountCents, 10, 64)
			if err == nil && cents > 0 {
				out.TopUp = &billingprovider.TopUpCredit{
					OrgID:       ev.Data.CustomData.OrgID,
					AmountCents: cents,
					Ref:         ev.Data.ID,
				}
			}
		}
	}
	return out, nil
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

// CreateCheckout creates a Paddle transaction for a prepaid credit top-up and
// returns its hosted checkout URL plus the customer id the transaction was
// created for. It POSTs to /transactions with the verified Paddle body shape: a
// single inline price item, collection_mode automatic, and custom_data carrying
// org_id and amount_cents for reconciliation. The response's data.customer_id
// echoes the request's customer_id when one was sent and is null for a guest
// checkout (Paddle creates the customer only when the hosted checkout collects
// an email), so Checkout.CustomerRef may be empty; the webhook-time link covers
// that case. The API key is sent as a bearer token and is never surfaced in any
// error.
func (p *Provider) CreateCheckout(ctx context.Context, in billingprovider.TopUp) (billingprovider.Checkout, error) {
	if p.apiKey == "" {
		return billingprovider.Checkout{}, errors.New("paddle: checkout not configured (no API key)")
	}
	centsStr := strconv.FormatInt(in.AmountCents, 10)
	type unitPrice struct {
		Amount       string `json:"amount"`
		CurrencyCode string `json:"currency_code"`
	}
	type priceQuantity struct {
		Minimum int `json:"minimum"`
		Maximum int `json:"maximum"`
	}
	type price struct {
		ProductID   string        `json:"product_id"`
		Description string        `json:"description"`
		UnitPrice   unitPrice     `json:"unit_price"`
		TaxMode     string        `json:"tax_mode"`
		Quantity    priceQuantity `json:"quantity"`
	}
	type item struct {
		Quantity int   `json:"quantity"`
		Price    price `json:"price"`
	}
	type customData struct {
		Kind        string `json:"kind"`
		OrgID       string `json:"org_id"`
		AmountCents string `json:"amount_cents"`
	}
	body := struct {
		Items          []item     `json:"items"`
		CollectionMode string     `json:"collection_mode"`
		CustomerID     string     `json:"customer_id,omitempty"`
		CustomData     customData `json:"custom_data"`
	}{
		Items: []item{
			{
				Quantity: 1,
				Price: price{
					ProductID:   in.ProductID,
					Description: "Credit top-up",
					UnitPrice: unitPrice{
						Amount:       centsStr,
						CurrencyCode: in.Currency,
					},
					TaxMode:  "account_setting",
					Quantity: priceQuantity{Minimum: 1, Maximum: 1},
				},
			},
		},
		CollectionMode: "automatic",
		CustomerID:     in.CustomerRef,
		CustomData: customData{
			Kind:        "credit_topup",
			OrgID:       in.OrgID,
			AmountCents: centsStr,
		},
	}
	bodyBytes, err := json.Marshal(body)
	if err != nil {
		return billingprovider.Checkout{}, fmt.Errorf("paddle: marshal checkout request: %w", err)
	}
	reqURL := p.baseURL + "/transactions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, bytes.NewReader(bodyBytes))
	if err != nil {
		return billingprovider.Checkout{}, fmt.Errorf("paddle: build checkout request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+p.apiKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.http.Do(req)
	if err != nil {
		return billingprovider.Checkout{}, fmt.Errorf("paddle: checkout request: %w", err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return billingprovider.Checkout{}, fmt.Errorf("paddle: read checkout response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return billingprovider.Checkout{}, fmt.Errorf("paddle: checkout API returned status %d", resp.StatusCode)
	}
	var out struct {
		Data struct {
			ID         string `json:"id"`
			CustomerID string `json:"customer_id"`
			Checkout   struct {
				URL string `json:"url"`
			} `json:"checkout"`
		} `json:"data"`
	}
	if err := json.Unmarshal(respBody, &out); err != nil {
		return billingprovider.Checkout{}, fmt.Errorf("paddle: malformed checkout response: %w", err)
	}
	if out.Data.Checkout.URL == "" {
		return billingprovider.Checkout{}, errors.New("paddle: checkout url missing; set a default payment link")
	}
	return billingprovider.Checkout{URL: out.Data.Checkout.URL, CustomerRef: out.Data.CustomerID}, nil
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
