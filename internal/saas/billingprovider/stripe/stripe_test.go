package stripe

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"mitos.run/mitos/internal/saas/billing"
	"mitos.run/mitos/internal/saas/billingprovider"
)

const secret = "whsec_test"

var now = time.Date(2026, 6, 22, 12, 0, 0, 0, time.UTC)

func sigHeader(sec string, t time.Time, body string) string {
	mac := hmac.New(sha256.New, []byte(sec))
	fmt.Fprintf(mac, "%d.%s", t.Unix(), body)
	return fmt.Sprintf("t=%d,v1=%s", t.Unix(), hex.EncodeToString(mac.Sum(nil)))
}

func provider() *Provider {
	return New(Config{SigningSecret: secret, Now: func() time.Time { return now }, Tolerance: 5 * time.Minute})
}

func verify(t *testing.T, sec string, ts time.Time, body string) (billingprovider.Event, error) {
	t.Helper()
	r := httptest.NewRequest("POST", "/webhooks/stripe", strings.NewReader(body))
	r.Header.Set("Stripe-Signature", sigHeader(sec, ts, body))
	return provider().VerifyWebhook(r, []byte(body))
}

// TestValidSignatureMapsPaymentFailed asserts a correctly-signed
// invoice.payment_failed maps to past_due with the customer ref extracted.
func TestValidSignatureMapsPaymentFailed(t *testing.T) {
	body := `{"type":"invoice.payment_failed","data":{"object":{"customer":"cus_x"}}}`
	ev, err := verify(t, secret, now, body)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if ev.Status != billing.StatusPastDue || ev.CustomerRef != "cus_x" {
		t.Fatalf("event = %+v, want past_due/cus_x", ev)
	}
}

// TestSubscriptionDeletedMapsSuspended covers the cancel path.
func TestSubscriptionDeletedMapsSuspended(t *testing.T) {
	body := `{"type":"customer.subscription.deleted","data":{"object":{"customer":"cus_x"}}}`
	ev, err := verify(t, secret, now, body)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if ev.Status != billing.StatusSuspended {
		t.Fatalf("status = %q, want suspended", ev.Status)
	}
}

// TestForgedSignatureRejected asserts a wrong-secret signature errors.
func TestForgedSignatureRejected(t *testing.T) {
	body := `{"type":"invoice.payment_failed","data":{"object":{"customer":"cus_x"}}}`
	if _, err := verify(t, "whsec_wrong", now, body); err == nil {
		t.Fatal("forged signature accepted")
	}
}

// TestStaleTimestampRejected asserts replay protection via the tolerance window.
func TestStaleTimestampRejected(t *testing.T) {
	body := `{"type":"invoice.payment_failed","data":{"object":{"customer":"cus_x"}}}`
	if _, err := verify(t, secret, now.Add(-time.Hour), body); err == nil {
		t.Fatal("stale signature accepted")
	}
}

// TestUnmappedEventHasNoStatus asserts an event type we don't act on verifies
// but yields an empty status (acknowledged, ignored).
func TestUnmappedEventHasNoStatus(t *testing.T) {
	body := `{"type":"charge.refunded","data":{"object":{"customer":"cus_x"}}}`
	ev, err := verify(t, secret, now, body)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if ev.Status != "" {
		t.Fatalf("status = %q, want empty for an unmapped event", ev.Status)
	}
}

// TestImplementsProvider is a compile-time seam check.
func TestImplementsProvider(t *testing.T) {
	var _ billingprovider.Provider = (*Provider)(nil)
}
