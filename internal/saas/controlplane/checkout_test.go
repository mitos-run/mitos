package controlplane

import (
	"testing"
	"time"
)

// checkoutCP builds a control plane with the checkout feature on for the
// "python" pool in single-tenant mode, the shape hosted prod runs.
func checkoutCP(t *testing.T) *K8sControlPlane {
	t.Helper()
	return New(newFakeClient(t),
		WithPollInterval(5*time.Millisecond),
		WithReadyTimeout(2*time.Second),
		WithSingleTenantNamespace("mitos"),
		WithCheckout(CheckoutConfig{Pools: []string{"python"}, Floor: 2, Cap: 4, MaxAge: 10 * time.Minute}),
	)
}

// TestCheckoutEligibilityGate pins which creates may be served from the
// buffer: a checkout-enabled pool with NO env, secrets, workspace, fan-out,
// or lifetime knobs. Everything else must take the classic path, because a
// buffered sandbox's activation handshake already ran with empty tenant
// inputs and its TTL clock would predate the claim.
func TestCheckoutEligibilityGate(t *testing.T) {
	cp := checkoutCP(t)
	b := cp.checkout
	if b == nil {
		t.Fatal("checkout buffer not constructed despite WithCheckout + single-tenant mode")
	}
	cases := []struct {
		name string
		body createBody
		pool string
		want bool
	}{
		{"plain create on a checkout pool", createBody{}, "python", true},
		{"pool not enabled", createBody{}, "default", false},
		{"env set", createBody{Env: map[string]string{"A": "b"}}, "python", false},
		{"secrets set", createBody{Secrets: []secretMountReq{{Name: "s"}}}, "python", false},
		{"workspace set", createBody{Workspace: "ws"}, "python", false},
		{"replicas fan-out", createBody{Replicas: 2}, "python", false},
		{"ttl set", createBody{TTL: "5m"}, "python", false},
		{"timeout set", createBody{Timeout: "5m"}, "python", false},
	}
	for _, tc := range cases {
		if got := b.eligible(tc.body, tc.pool); got != tc.want {
			t.Errorf("%s: eligible = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestCheckoutRequiresSingleTenantNamespace asserts the buffer is NOT
// constructed under per-org namespacing: the design leans on the shared
// namespace (see the spec's migration note) and must fail safe to the
// classic path everywhere else.
func TestCheckoutRequiresSingleTenantNamespace(t *testing.T) {
	cp := New(newFakeClient(t), WithCheckout(CheckoutConfig{Pools: []string{"python"}}))
	if cp.checkout != nil {
		t.Fatal("checkout buffer constructed without single-tenant namespace mode")
	}
}
