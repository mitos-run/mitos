package paddle

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"mitos.run/mitos/internal/saas/billing"
	"mitos.run/mitos/internal/saas/billingprovider"
)

const secret = "pdl_ntfset_test"

var now = time.Date(2026, 6, 26, 12, 0, 0, 0, time.UTC)

// sigHeader computes a valid Paddle-Signature header for body at time t with the
// given secret: ts=<unix>;h1=hex(HMAC_SHA256(secret, "<ts>:<body>")).
func sigHeader(sec string, t time.Time, body string) string {
	mac := hmac.New(sha256.New, []byte(sec))
	fmt.Fprintf(mac, "%d:%s", t.Unix(), body)
	return fmt.Sprintf("ts=%d;h1=%s", t.Unix(), hex.EncodeToString(mac.Sum(nil)))
}

func provider() *Provider {
	return New(Config{
		WebhookSecret: secret,
		Now:           func() time.Time { return now },
		Tolerance:     5 * time.Minute,
	})
}

func verifyWith(t *testing.T, header, body string) (billingprovider.Event, error) {
	t.Helper()
	r := httptest.NewRequest("POST", "/webhooks/billing", strings.NewReader(body))
	if header != "" {
		r.Header.Set("Paddle-Signature", header)
	}
	return provider().VerifyWebhook(r, []byte(body))
}

func verify(t *testing.T, sec string, ts time.Time, body string) (billingprovider.Event, error) {
	t.Helper()
	return verifyWith(t, sigHeader(sec, ts, body), body)
}

// TestImplementsProvider is a compile-time seam check.
func TestImplementsProvider(t *testing.T) {
	var _ billingprovider.Provider = (*Provider)(nil)
}

// TestValidSignaturePastDue asserts a correctly-signed subscription.past_due maps
// to past_due with the customer ref extracted.
func TestValidSignaturePastDue(t *testing.T) {
	body := `{"event_type":"subscription.past_due","data":{"customer_id":"ctm_x"}}`
	ev, err := verify(t, secret, now, body)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if ev.Status != billing.StatusPastDue || ev.CustomerRef != "ctm_x" {
		t.Fatalf("event = %+v, want past_due/ctm_x", ev)
	}
}

// TestTamperedBodyRejected asserts a body changed after signing fails (the MAC no
// longer matches).
func TestTamperedBodyRejected(t *testing.T) {
	signed := `{"event_type":"subscription.past_due","data":{"customer_id":"ctm_x"}}`
	header := sigHeader(secret, now, signed)
	tampered := `{"event_type":"subscription.activated","data":{"customer_id":"ctm_x"}}`
	if _, err := verifyWith(t, header, tampered); err == nil {
		t.Fatal("tampered body accepted")
	}
}

// TestForgedSignatureRejected asserts a wrong-secret signature errors.
func TestForgedSignatureRejected(t *testing.T) {
	body := `{"event_type":"subscription.past_due","data":{"customer_id":"ctm_x"}}`
	if _, err := verify(t, "pdl_ntfset_wrong", now, body); err == nil {
		t.Fatal("forged signature accepted")
	}
}

// TestMissingSignatureRejected asserts a request with no Paddle-Signature header
// is refused.
func TestMissingSignatureRejected(t *testing.T) {
	body := `{"event_type":"subscription.past_due","data":{"customer_id":"ctm_x"}}`
	if _, err := verifyWith(t, "", body); err == nil {
		t.Fatal("missing signature accepted")
	}
}

// TestMalformedSignatureRejected asserts a header that lacks ts/h1 is refused.
func TestMalformedSignatureRejected(t *testing.T) {
	body := `{"event_type":"subscription.past_due","data":{"customer_id":"ctm_x"}}`
	if _, err := verifyWith(t, "garbage", body); err == nil {
		t.Fatal("malformed signature accepted")
	}
}

// TestStaleTimestampRejected asserts replay protection via the tolerance window.
func TestStaleTimestampRejected(t *testing.T) {
	body := `{"event_type":"subscription.past_due","data":{"customer_id":"ctm_x"}}`
	if _, err := verify(t, secret, now.Add(-time.Hour), body); err == nil {
		t.Fatal("stale signature accepted")
	}
}

// TestEmptySecretFailsClosed asserts a provider with no signing secret refuses
// every webhook, even one carrying a structurally valid header.
func TestEmptySecretFailsClosed(t *testing.T) {
	p := New(Config{Now: func() time.Time { return now }})
	body := `{"event_type":"subscription.activated","data":{"customer_id":"ctm_x"}}`
	r := httptest.NewRequest("POST", "/webhooks/billing", strings.NewReader(body))
	r.Header.Set("Paddle-Signature", sigHeader(secret, now, body))
	if _, err := p.VerifyWebhook(r, []byte(body)); err == nil {
		t.Fatal("empty secret accepted a webhook; must fail closed")
	}
}

// TestEventMapping covers the representative Paddle event types and the neutral
// status each drives, matching the Stripe provider's semantics.
func TestEventMapping(t *testing.T) {
	cases := []struct {
		eventType string
		want      billing.BillingStatus
	}{
		{"subscription.created", billing.StatusActive},
		{"subscription.activated", billing.StatusActive},
		{"subscription.resumed", billing.StatusActive},
		{"subscription.updated", billing.StatusActive},
		{"transaction.completed", billing.StatusActive},
		{"transaction.paid", billing.StatusActive},
		{"subscription.past_due", billing.StatusPastDue},
		{"transaction.payment_failed", billing.StatusPastDue},
		{"subscription.canceled", billing.StatusSuspended},
		{"subscription.paused", billing.StatusSuspended},
		{"customer.updated", ""}, // unmapped: acknowledged, ignored
	}
	for _, tc := range cases {
		t.Run(tc.eventType, func(t *testing.T) {
			body := fmt.Sprintf(`{"event_type":%q,"data":{"customer_id":"ctm_x"}}`, tc.eventType)
			ev, err := verify(t, secret, now, body)
			if err != nil {
				t.Fatalf("verify: %v", err)
			}
			if ev.Status != tc.want {
				t.Fatalf("%s -> %q, want %q", tc.eventType, ev.Status, tc.want)
			}
		})
	}
}

// TestNegativeToleranceDisablesTimestampCheck asserts a negative tolerance allows
// an old-but-correctly-signed event (the check is opt-out).
func TestNegativeToleranceDisablesTimestampCheck(t *testing.T) {
	p := New(Config{WebhookSecret: secret, Now: func() time.Time { return now }, Tolerance: -1})
	body := `{"event_type":"subscription.activated","data":{"customer_id":"ctm_x"}}`
	r := httptest.NewRequest("POST", "/webhooks/billing", strings.NewReader(body))
	r.Header.Set("Paddle-Signature", sigHeader(secret, now.Add(-72*time.Hour), body))
	if _, err := p.VerifyWebhook(r, []byte(body)); err != nil {
		t.Fatalf("negative tolerance still enforced timestamp: %v", err)
	}
}

// TestPortalURL asserts PortalURL POSTs to the correct path with the bearer key
// and parses the overview URL from the Paddle response.
func TestPortalURL(t *testing.T) {
	const apiKey = "pdl_live_secretkey"
	var gotPath, gotMethod, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotMethod = r.Method
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"urls":{"general":{"overview":"https://customer.paddle.com/session/abc"}}}}`))
	}))
	defer srv.Close()

	p := New(Config{APIKey: apiKey, BaseURL: srv.URL})
	url, err := p.PortalURL(context.Background(), "ctm_42")
	if err != nil {
		t.Fatalf("PortalURL: %v", err)
	}
	if url != "https://customer.paddle.com/session/abc" {
		t.Fatalf("url = %q", url)
	}
	if gotMethod != http.MethodPost {
		t.Fatalf("method = %q, want POST", gotMethod)
	}
	if gotPath != "/customers/ctm_42/portal-sessions" {
		t.Fatalf("path = %q", gotPath)
	}
	if gotAuth != "Bearer "+apiKey {
		t.Fatalf("auth header = %q", gotAuth)
	}
}

// TestPortalURLNotConfigured asserts the portal call errors when no API key is
// set (the console then hides the manage-subscription affordance).
func TestPortalURLNotConfigured(t *testing.T) {
	p := New(Config{})
	if _, err := p.PortalURL(context.Background(), "ctm_42"); err == nil {
		t.Fatal("portal returned a URL without an API key")
	}
}

// TestPortalURLAPIError asserts a non-2xx Paddle response surfaces as a wrapped
// Go error WITHOUT leaking the API key.
func TestPortalURLAPIError(t *testing.T) {
	const apiKey = "pdl_live_topsecret_DEADBEEF"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"detail":"forbidden"}}`))
	}))
	defer srv.Close()

	p := New(Config{APIKey: apiKey, BaseURL: srv.URL})
	_, err := p.PortalURL(context.Background(), "ctm_42")
	if err == nil {
		t.Fatal("expected an error for a 403 response")
	}
	if strings.Contains(err.Error(), apiKey) {
		t.Fatalf("API key leaked in error: %v", err)
	}
}

// TestAPIKeyNeverInErrors is the secret-hygiene gate: across every error path the
// PortalURL surface can take, the API key must never appear in the returned error
// string.
func TestAPIKeyNeverInErrors(t *testing.T) {
	const apiKey = "pdl_live_NEVER_LOG_ME_1234567890"

	// 1. Non-2xx response.
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer bad.Close()
	// 2. Malformed JSON response.
	malformed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("not json"))
	}))
	defer malformed.Close()
	// 3. Empty overview URL.
	empty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":{"urls":{"general":{"overview":""}}}}`))
	}))
	defer empty.Close()

	for _, base := range []string{bad.URL, malformed.URL, empty.URL, "http://127.0.0.1:1"} {
		p := New(Config{APIKey: apiKey, BaseURL: base, HTTPClient: &http.Client{Timeout: time.Second}})
		_, err := p.PortalURL(context.Background(), "ctm_42")
		if err == nil {
			continue
		}
		if strings.Contains(err.Error(), apiKey) {
			t.Fatalf("API key leaked in error for base %s: %v", base, err)
		}
	}
}

// TestVerifyWebhookCarriesOrgID asserts the org id embedded in the
// signature-verified custom_data at checkout is surfaced on the neutral Event
// (Event.OrgID), so the webhook handler can record the org <-> customer link
// on the very first event for a new customer (issue #618).
func TestVerifyWebhookCarriesOrgID(t *testing.T) {
	body := `{"event_type":"transaction.completed","data":{"id":"txn_1","customer_id":"ctm_x","custom_data":{"kind":"credit_topup","org_id":"orgA","amount_cents":"2500"}}}`
	ev, err := verify(t, secret, now, body)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if ev.OrgID != "orgA" {
		t.Fatalf("OrgID = %q, want \"orgA\"", ev.OrgID)
	}
	if ev.CustomerRef != "ctm_x" {
		t.Fatalf("CustomerRef = %q, want \"ctm_x\"", ev.CustomerRef)
	}
}

// TestVerifyWebhookOrgIDAbsent asserts an event without custom_data leaves
// Event.OrgID empty (the handler then resolves the org via the stored link).
func TestVerifyWebhookOrgIDAbsent(t *testing.T) {
	body := `{"event_type":"subscription.canceled","data":{"customer_id":"ctm_x"}}`
	ev, err := verify(t, secret, now, body)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if ev.OrgID != "" {
		t.Fatalf("OrgID = %q, want empty when custom_data is absent", ev.OrgID)
	}
}

// TestCheckoutURL asserts CreateCheckout POSTs a transaction to Paddle with the
// correct bearer auth, verified request body shape, and parses the checkout URL
// plus the customer id the transaction was created for.
func TestCheckoutURL(t *testing.T) {
	const apiKey = "pdl_live_checkoutkey"
	const productID = "pro_topup"
	const orgID = "org_abc"
	const customerRef = "ctm_42"
	var amountCents int64 = 5000 // EUR 50.00

	var gotPath, gotMethod, gotAuth string
	var gotBody map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotMethod = r.Method
		gotAuth = r.Header.Get("Authorization")
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"id":"txn_x","customer_id":"ctm_42","checkout":{"url":"https://example/checkout?_ptxn=txn_x"}}}`))
	}))
	defer srv.Close()

	p := New(Config{APIKey: apiKey, BaseURL: srv.URL})
	co, err := p.CreateCheckout(context.Background(), billingprovider.TopUp{
		CustomerRef: customerRef,
		OrgID:       orgID,
		AmountCents: amountCents,
		ProductID:   productID,
		Currency:    "EUR",
	})
	if err != nil {
		t.Fatalf("CreateCheckout: %v", err)
	}
	if co.URL != "https://example/checkout?_ptxn=txn_x" {
		t.Fatalf("url = %q, want checkout URL", co.URL)
	}
	if co.CustomerRef != "ctm_42" {
		t.Fatalf("CustomerRef = %q, want \"ctm_42\" from the response body", co.CustomerRef)
	}
	if gotMethod != http.MethodPost {
		t.Fatalf("method = %q, want POST", gotMethod)
	}
	if gotPath != "/transactions" {
		t.Fatalf("path = %q, want /transactions", gotPath)
	}
	if gotAuth != "Bearer "+apiKey {
		t.Fatalf("auth = %q, want Bearer key", gotAuth)
	}
	// Assert the request body has the verified Paddle shape.
	items, ok := gotBody["items"].([]any)
	if !ok || len(items) == 0 {
		t.Fatal("items missing or empty in request body")
	}
	item, ok := items[0].(map[string]any)
	if !ok {
		t.Fatal("items[0] is not an object")
	}
	price, ok := item["price"].(map[string]any)
	if !ok {
		t.Fatal("items[0].price is not an object")
	}
	if price["product_id"] != productID {
		t.Fatalf("product_id = %v, want %q", price["product_id"], productID)
	}
	unitPrice, ok := price["unit_price"].(map[string]any)
	if !ok {
		t.Fatal("unit_price is not an object")
	}
	if unitPrice["amount"] != "5000" {
		t.Fatalf("unit_price.amount = %v, want \"5000\"", unitPrice["amount"])
	}
	if unitPrice["currency_code"] != "EUR" {
		t.Fatalf("currency_code = %v, want \"EUR\"", unitPrice["currency_code"])
	}
	customData, ok := gotBody["custom_data"].(map[string]any)
	if !ok {
		t.Fatal("custom_data missing in request body")
	}
	if customData["org_id"] != orgID {
		t.Fatalf("custom_data.org_id = %v, want %q", customData["org_id"], orgID)
	}
	if customData["amount_cents"] != "5000" {
		t.Fatalf("custom_data.amount_cents = %v, want \"5000\"", customData["amount_cents"])
	}
	if customData["kind"] != "credit_topup" {
		t.Fatalf("custom_data.kind = %v, want \"credit_topup\"", customData["kind"])
	}
	// customer_id must be forwarded when provided.
	if gotBody["customer_id"] != customerRef {
		t.Fatalf("customer_id = %v, want %q", gotBody["customer_id"], customerRef)
	}
}

// TestCheckoutURLOmitsEmptyCustomerRef asserts customer_id is absent when
// CustomerRef is empty (guest checkout).
func TestCheckoutURLOmitsEmptyCustomerRef(t *testing.T) {
	const apiKey = "pdl_live_checkoutkey"
	var gotBody map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"id":"txn_y","checkout":{"url":"https://example/checkout?_ptxn=txn_y"}}}`))
	}))
	defer srv.Close()

	p := New(Config{APIKey: apiKey, BaseURL: srv.URL})
	co, err := p.CreateCheckout(context.Background(), billingprovider.TopUp{
		OrgID:       "org_abc",
		AmountCents: 1000,
		ProductID:   "pro_topup",
		Currency:    "EUR",
	})
	if err != nil {
		t.Fatalf("CreateCheckout: %v", err)
	}
	if _, present := gotBody["customer_id"]; present {
		t.Fatalf("customer_id should be absent for empty CustomerRef, got %v", gotBody["customer_id"])
	}
	// The stub response carries no customer_id (a first-time buyer: Paddle
	// creates the customer only when the hosted checkout collects an email),
	// so the parsed CustomerRef must be empty; the webhook path links later.
	if co.CustomerRef != "" {
		t.Fatalf("CustomerRef = %q, want empty when the response carries none", co.CustomerRef)
	}
}

// TestCheckoutURLNon2xx asserts a non-2xx Paddle response surfaces as a wrapped
// error without leaking the API key.
func TestCheckoutURLNon2xx(t *testing.T) {
	const apiKey = "pdl_live_checkout_secret_NEVER"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"detail":"forbidden"}}`))
	}))
	defer srv.Close()

	p := New(Config{APIKey: apiKey, BaseURL: srv.URL})
	_, err := p.CreateCheckout(context.Background(), billingprovider.TopUp{
		OrgID:       "org_abc",
		AmountCents: 5000,
		ProductID:   "pro_topup",
		Currency:    "EUR",
	})
	if err == nil {
		t.Fatal("expected error for 403 response")
	}
	if strings.Contains(err.Error(), apiKey) {
		t.Fatalf("API key leaked in error: %v", err)
	}
}

// TestCheckoutURLEmptyURL asserts an empty checkout.url in the Paddle response
// yields a clear wrapped error with no secret leak.
func TestCheckoutURLEmptyURL(t *testing.T) {
	const apiKey = "pdl_live_checkout_secret_NEVER"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"id":"txn_x","checkout":{"url":""}}}`))
	}))
	defer srv.Close()

	p := New(Config{APIKey: apiKey, BaseURL: srv.URL})
	_, err := p.CreateCheckout(context.Background(), billingprovider.TopUp{
		OrgID:       "org_abc",
		AmountCents: 5000,
		ProductID:   "pro_topup",
		Currency:    "EUR",
	})
	if err == nil {
		t.Fatal("expected error for empty checkout URL in response")
	}
	if strings.Contains(err.Error(), apiKey) {
		t.Fatalf("API key leaked in error: %v", err)
	}
}

// TestVerifyWebhookTopUpCredit asserts that a signed transaction.completed body
// carrying custom_data.kind="credit_topup" produces a non-nil ev.TopUp with the
// correct OrgID, AmountCents, and Ref, AND maps StatusActive.
func TestVerifyWebhookTopUpCredit(t *testing.T) {
	body := `{"event_type":"transaction.completed","data":{"id":"txn_9","customer_id":"ctm_x","custom_data":{"kind":"credit_topup","org_id":"orgA","amount_cents":"2500"}}}`
	ev, err := verify(t, secret, now, body)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if ev.TopUp == nil {
		t.Fatal("ev.TopUp is nil, want non-nil for credit_topup event")
	}
	if ev.TopUp.OrgID != "orgA" {
		t.Fatalf("TopUp.OrgID = %q, want \"orgA\"", ev.TopUp.OrgID)
	}
	if ev.TopUp.AmountCents != 2500 {
		t.Fatalf("TopUp.AmountCents = %d, want 2500", ev.TopUp.AmountCents)
	}
	if ev.TopUp.Ref != "txn_9" {
		t.Fatalf("TopUp.Ref = %q, want \"txn_9\"", ev.TopUp.Ref)
	}
	// A credit top-up carries no subscription-state meaning, so it must NOT move
	// the billing status (else buying credits could clear an org's dunning).
	if ev.Status != "" {
		t.Fatalf("Status = %q, want empty (a credit top-up must not change status)", ev.Status)
	}
}

// TestVerifyWebhookTopUpPaidEvent asserts the transaction.paid event type also
// yields a top-up credit (the completed/paid branch is an OR), so a provider that
// emits paid instead of completed still credits the org.
func TestVerifyWebhookTopUpPaidEvent(t *testing.T) {
	body := `{"event_type":"transaction.paid","data":{"id":"txn_paid","customer_id":"ctm_x","custom_data":{"kind":"credit_topup","org_id":"orgB","amount_cents":"7500"}}}`
	ev, err := verify(t, secret, now, body)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if ev.TopUp == nil {
		t.Fatal("ev.TopUp is nil, want non-nil for a paid credit_topup event")
	}
	if ev.TopUp.OrgID != "orgB" || ev.TopUp.AmountCents != 7500 || ev.TopUp.Ref != "txn_paid" {
		t.Fatalf("TopUp = %+v, want {orgB 7500 txn_paid}", ev.TopUp)
	}
	if ev.Status != "" {
		t.Fatalf("Status = %q, want empty (a credit top-up must not change status)", ev.Status)
	}
}

// TestVerifyWebhookTopUpKindAbsent asserts that a transaction.completed body
// without the credit_topup kind leaves ev.TopUp nil (normal transaction, not a
// credit purchase) AND keeps its subscription status, so a genuine subscription
// payment still marks the org active. Only a credit_topup transaction suppresses
// the status.
func TestVerifyWebhookTopUpKindAbsent(t *testing.T) {
	body := `{"event_type":"transaction.completed","data":{"id":"txn_10","customer_id":"ctm_x"}}`
	ev, err := verify(t, secret, now, body)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if ev.TopUp != nil {
		t.Fatalf("ev.TopUp = %+v, want nil when kind is absent", ev.TopUp)
	}
	if ev.Status != billing.StatusActive {
		t.Fatalf("Status = %q, want StatusActive for a plain transaction", ev.Status)
	}
}

// TestVerifyWebhookTopUpMalformedAmount asserts that a malformed amount_cents
// leaves ev.TopUp nil and does NOT return a verification error (the event is
// still acknowledged). The status is still suppressed: a malformed top-up is
// still a credit_topup transaction, not a subscription signal, so it must not
// move the billing status either.
func TestVerifyWebhookTopUpMalformedAmount(t *testing.T) {
	body := `{"event_type":"transaction.completed","data":{"id":"txn_11","customer_id":"ctm_x","custom_data":{"kind":"credit_topup","org_id":"orgA","amount_cents":"abc"}}}`
	ev, err := verify(t, secret, now, body)
	if err != nil {
		t.Fatalf("verify returned error for malformed amount: %v", err)
	}
	if ev.TopUp != nil {
		t.Fatalf("ev.TopUp = %+v, want nil for malformed amount", ev.TopUp)
	}
	if ev.Status != "" {
		t.Fatalf("Status = %q, want empty (a credit_topup transaction never changes status)", ev.Status)
	}
}

// TestCheckoutURLKeyNeverInErrors is the secret-hygiene gate for CheckoutURL:
// the API key must never appear in any returned error string.
func TestCheckoutURLKeyNeverInErrors(t *testing.T) {
	const apiKey = "pdl_live_CHECKOUT_NEVER_LOG_ME_1234567890"

	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer bad.Close()
	malformed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("not json"))
	}))
	defer malformed.Close()
	emptyURL := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":{"id":"txn_x","checkout":{"url":""}}}`))
	}))
	defer emptyURL.Close()

	for _, base := range []string{bad.URL, malformed.URL, emptyURL.URL, "http://127.0.0.1:1"} {
		p := New(Config{APIKey: apiKey, BaseURL: base, HTTPClient: &http.Client{Timeout: time.Second}})
		_, err := p.CreateCheckout(context.Background(), billingprovider.TopUp{
			OrgID:       "org_abc",
			AmountCents: 5000,
			ProductID:   "pro_topup",
			Currency:    "EUR",
		})
		if err == nil {
			continue
		}
		if strings.Contains(err.Error(), apiKey) {
			t.Fatalf("API key leaked in error for base %s: %v", base, err)
		}
	}
}
