package paddle

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
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
