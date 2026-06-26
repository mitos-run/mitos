package saas

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ipCapturingQuota records the client IP the gateway placed in the request
// context, so a test can assert the gateway resolves and plumbs the trusted IP
// down to the QuotaEnforcer seam (where the real enforcer's per-IP bucket reads
// it via ClientIPFromContext).
type ipCapturingQuota struct{ ip string }

func (q *ipCapturingQuota) Check(ctx context.Context, _, _ string) error {
	q.ip = ClientIPFromContext(ctx)
	return nil
}

// TestGatewayPlumbsTrustedClientIPToQuota asserts the gateway resolves the client
// IP and exposes it to the QuotaEnforcer via the context. With zero trusted hops
// (the default) a spoofed X-Forwarded-For is ignored and the RemoteAddr host is
// used, so the per-IP rate-limit bucket cannot be moved off the real source.
func TestGatewayPlumbsTrustedClientIPToQuota(t *testing.T) {
	store := NewMemStore()
	newTestOrg(t, store, "org-a")
	keys := NewKeyService(store)
	created, err := keys.CreateKey(context.Background(), CreateKeyRequest{OrgID: "org-a", Scopes: []string{ScopeReadOnly}})
	if err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	q := &ipCapturingQuota{}
	gw := NewGateway(keys, q, &fakeControlPlane{respBody: []byte(`[]`)}, nil)

	req := httptest.NewRequest(http.MethodGet, "/v1/sandboxes", strings.NewReader(""))
	req.Header.Set("Authorization", "Bearer "+created.RawKey)
	req.RemoteAddr = "203.0.113.50:9999"
	req.Header.Set("X-Forwarded-For", "1.2.3.4") // attacker-set, must be ignored.
	gw.ServeHTTP(httptest.NewRecorder(), req)

	if q.ip != "203.0.113.50" {
		t.Fatalf("quota saw client IP %q, want 203.0.113.50 (RemoteAddr); a spoofed XFF must not win", q.ip)
	}
}

// TestGatewayTrustsConfiguredProxyHopForClientIP asserts that when the gateway is
// configured with one trusted proxy hop it uses the rightmost X-Forwarded-For
// entry (the address the trusted ingress observed) as the client IP.
func TestGatewayTrustsConfiguredProxyHopForClientIP(t *testing.T) {
	store := NewMemStore()
	newTestOrg(t, store, "org-a")
	keys := NewKeyService(store)
	created, err := keys.CreateKey(context.Background(), CreateKeyRequest{OrgID: "org-a", Scopes: []string{ScopeReadOnly}})
	if err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	q := &ipCapturingQuota{}
	gw := NewGateway(keys, q, &fakeControlPlane{respBody: []byte(`[]`)}, nil).WithTrustedProxyHops(1)

	req := httptest.NewRequest(http.MethodGet, "/v1/sandboxes", strings.NewReader(""))
	req.Header.Set("Authorization", "Bearer "+created.RawKey)
	req.RemoteAddr = "10.0.0.1:9999"                           // the trusted ingress.
	req.Header.Set("X-Forwarded-For", "1.2.3.4, 198.51.100.9") // attacker-prepended, trusted-appended.
	gw.ServeHTTP(httptest.NewRecorder(), req)

	if q.ip != "198.51.100.9" {
		t.Fatalf("quota saw client IP %q, want 198.51.100.9 (rightmost, trusted-observed)", q.ip)
	}
}
