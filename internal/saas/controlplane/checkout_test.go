package controlplane

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	v1 "mitos.run/mitos/api/v1"
	"mitos.run/mitos/internal/saas"
	"mitos.run/mitos/internal/tenant"
)

// poolInNs builds a SandboxPool in an explicit namespace (the single-tenant
// shape, unlike poolIn's per-org namespace derivation).
func poolInNs(ns, name string) *v1.SandboxPool {
	return &v1.SandboxPool{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}
}

// seedBuffered creates a Ready buffered sandbox CR in ns "mitos" and returns
// the entry the gateway would cache for it.
func seedBuffered(t *testing.T, c client.Client, name string) bufferedSandbox {
	t.Helper()
	sb := &v1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "mitos",
			Labels: map[string]string{BufferedLabelKey: "true", bufferedPoolLabelKey: "python"},
			// The fake client does not stamp CreationTimestamp (a real
			// apiserver always does), and the reap path needs a real age.
			CreationTimestamp: metav1.Now(),
		},
		Spec: v1.SandboxSpec{Source: v1.SandboxSource{PoolRef: &v1.LocalObjectReference{Name: "python"}}},
	}
	if err := c.Create(context.Background(), sb); err != nil {
		t.Fatalf("seed buffered sandbox: %v", err)
	}
	sb.Status.Phase = v1.SandboxReady
	sb.Status.Endpoint = "10.0.0.9:9091"
	sb.Status.Pod = name
	if err := c.Status().Update(context.Background(), sb); err != nil {
		t.Fatalf("seed status: %v", err)
	}
	var cur v1.Sandbox
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "mitos", Name: name}, &cur); err != nil {
		t.Fatalf("re-get: %v", err)
	}
	return bufferedSandbox{
		name: name, pool: "python", endpoint: "10.0.0.9:9091", token: "tok-" + name,
		resourceVersion: cur.ResourceVersion, podName: name, createdAt: time.Now(),
	}
}

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

// TestCheckoutServesCreateFromBuffer asserts an eligible create is served
// from the buffer: 201 with the cached endpoint and token, NO Sandbox Create
// on the hot path, and the CR atomically loses the buffered label and gains
// the caller's org labels BEFORE the response returns (runtime authz getOwned
// re-checks that label on the first exec).
func TestCheckoutServesCreateFromBuffer(t *testing.T) {
	var creates atomic.Int64
	base := fakeclient.NewClientBuilder().
		WithScheme(newScheme(t)).
		WithStatusSubresource(&v1.Sandbox{}).
		Build()
	c := interceptor.NewClient(base, interceptor.Funcs{
		Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if _, ok := obj.(*v1.Sandbox); ok {
				creates.Add(1)
			}
			return cl.Create(ctx, obj, opts...)
		},
	})
	cp := New(c,
		WithPollInterval(5*time.Millisecond), WithReadyTimeout(2*time.Second),
		WithSingleTenantNamespace("mitos"),
		WithCheckout(CheckoutConfig{Pools: []string{"python"}}))
	cp.checkout.add(seedBuffered(t, c, "sb-buffered1"))
	creates.Store(0)

	resp, err := cp.Forward(context.Background(), saas.ForwardRequest{
		OrgID: orgA, Op: "sandbox.create", Body: []byte(`{"pool":"python"}`),
	})
	if err != nil {
		t.Fatalf("Forward: %v", err)
	}
	if resp.Status != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", resp.Status, resp.Body)
	}
	m := decodeBody(t, resp.Body)
	if m["id"] != "sb-buffered1" || m["endpoint"] != "10.0.0.9:9091" || m["token"] != "tok-sb-buffered1" {
		t.Fatalf("payload = %v, want the buffered sandbox's identity", m)
	}
	if n := creates.Load(); n != 0 {
		t.Fatalf("checkout performed %d Sandbox Create(s) on the hot path, want 0", n)
	}
	var sb v1.Sandbox
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "mitos", Name: "sb-buffered1"}, &sb); err != nil {
		t.Fatalf("get: %v", err)
	}
	if _, still := sb.Labels[BufferedLabelKey]; still {
		t.Error("buffered label survived the checkout")
	}
	if sb.Labels[tenant.OrgLabelKey] != orgA {
		t.Errorf("org label = %q, want %q", sb.Labels[tenant.OrgLabelKey], orgA)
	}
}

// TestCheckoutIsExclusiveAcrossReplicas asserts two gateways holding the same
// cached entry cannot both hand it out: the resourceVersion-guarded patch lets
// exactly one win, and the loser falls back to the classic path.
func TestCheckoutIsExclusiveAcrossReplicas(t *testing.T) {
	c := newFakeClient(t, poolInNs("mitos", "python"))
	mk := func() *K8sControlPlane {
		return New(c,
			WithPollInterval(5*time.Millisecond), WithReadyTimeout(2*time.Second),
			WithSingleTenantNamespace("mitos"),
			WithCheckout(CheckoutConfig{Pools: []string{"python"}}))
	}
	cpA, cpB := mk(), mk()
	e := seedBuffered(t, c, "sb-shared")
	cpA.checkout.add(e)
	cpB.checkout.add(e)

	stop := flipToReadyWhenCreatedInNs(t, c, "mitos", "10.9.9.9:9091", "tok-classic")
	defer stop()

	respA, errA := cpA.Forward(context.Background(), saas.ForwardRequest{OrgID: orgA, Op: "sandbox.create", Body: []byte(`{"pool":"python"}`)})
	respB, errB := cpB.Forward(context.Background(), saas.ForwardRequest{OrgID: orgB, Op: "sandbox.create", Body: []byte(`{"pool":"python"}`)})
	if errA != nil || errB != nil {
		t.Fatalf("Forward: %v / %v", errA, errB)
	}
	if respA.Status != http.StatusCreated || respB.Status != http.StatusCreated {
		t.Fatalf("statuses = %d/%d, bodies %s / %s", respA.Status, respB.Status, respA.Body, respB.Body)
	}
	idA := decodeBody(t, respA.Body)["id"]
	idB := decodeBody(t, respB.Body)["id"]
	got := 0
	if idA == "sb-shared" {
		got++
	}
	if idB == "sb-shared" {
		got++
	}
	if got != 1 {
		t.Fatalf("the shared buffered sandbox was handed out %d times (ids %v, %v), want exactly 1", got, idA, idB)
	}
}

// TestCheckoutEmptyBufferFallsBackClassic asserts an eligible create with no
// buffered entry takes the classic path and still succeeds.
func TestCheckoutEmptyBufferFallsBackClassic(t *testing.T) {
	c := newFakeClient(t, poolInNs("mitos", "python"))
	cp := New(c,
		WithPollInterval(5*time.Millisecond), WithReadyTimeout(2*time.Second),
		WithSingleTenantNamespace("mitos"),
		WithCheckout(CheckoutConfig{Pools: []string{"python"}}))
	stop := flipToReadyWhenCreatedInNs(t, c, "mitos", "10.9.9.9:9091", "tok-classic")
	defer stop()
	resp, err := cp.Forward(context.Background(), saas.ForwardRequest{OrgID: orgA, Op: "sandbox.create", Body: []byte(`{"pool":"python"}`)})
	if err != nil {
		t.Fatalf("Forward: %v", err)
	}
	if resp.Status != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", resp.Status, resp.Body)
	}
}

// TestRefillFillsToFloorAndStopsAtIt asserts reconcilePool creates buffered
// sandboxes (org-less, buffered+pool labels) until the cluster count reaches
// the floor, one per pass so two replicas converge without a thundering herd,
// and a pass at the floor creates none.
func TestRefillFillsToFloorAndStopsAtIt(t *testing.T) {
	c := newFakeClient(t, poolInNs("mitos", "python"))
	cp := New(c,
		WithPollInterval(5*time.Millisecond), WithReadyTimeout(2*time.Second),
		WithSingleTenantNamespace("mitos"),
		WithCheckout(CheckoutConfig{Pools: []string{"python"}, Floor: 2, Cap: 4, MaxAge: 10 * time.Minute}))

	countBuffered := func() int {
		var list v1.SandboxList
		if err := c.List(context.Background(), &list, client.InNamespace("mitos"),
			client.MatchingLabels{BufferedLabelKey: "true"}); err != nil {
			t.Fatalf("list: %v", err)
		}
		return len(list.Items)
	}

	for i := 0; i < 2; i++ {
		stop := flipToReadyWhenCreatedInNs(t, c, "mitos", "10.0.0.5:9091", "tok-refill")
		// The keepalive is exercised by the checkout_keepalive tests; here it
		// must not dial the fake endpoints, so warming is a no-op.
		cp.checkout.warm = noopWarm
		cp.checkout.reconcilePool(context.Background(), "python")
		stop()
	}
	if n := countBuffered(); n != 2 {
		t.Fatalf("buffered count after two passes = %d, want floor 2", n)
	}
	cp.checkout.reconcilePool(context.Background(), "python")
	if n := countBuffered(); n != 2 {
		t.Fatalf("buffered count after an at-floor pass = %d, want 2 (no over-refill)", n)
	}
	e, ok := cp.checkout.pop("python")
	if !ok {
		t.Fatal("refill did not cache a claimable entry")
	}
	if e.token != "tok-refill" || e.endpoint != "10.0.0.5:9091" || e.resourceVersion == "" {
		t.Fatalf("cached entry incomplete: %+v", e)
	}
	// The buffered CRs carry NO org label: they bill nobody and match no caller.
	var list v1.SandboxList
	_ = c.List(context.Background(), &list, client.InNamespace("mitos"), client.MatchingLabels{BufferedLabelKey: "true"})
	for _, sb := range list.Items {
		if sb.Labels[tenant.OrgLabelKey] != "" {
			t.Fatalf("buffered sandbox %s carries an org label %q", sb.Name, sb.Labels[tenant.OrgLabelKey])
		}
	}
}

// TestRefillBacksOffAfterFailure asserts a failed refill sets a backoff: the
// next pass makes NO create attempt until the backoff expires (the #894
// lesson: a refill that cannot succeed must never spin at full cadence). The
// create itself errors so no CR lingers to satisfy the floor by accident;
// only the backoff can explain the second pass staying quiet.
func TestRefillBacksOffAfterFailure(t *testing.T) {
	var creates atomic.Int64
	base := fakeclient.NewClientBuilder().
		WithScheme(newScheme(t)).
		WithStatusSubresource(&v1.Sandbox{}).
		WithObjects(poolInNs("mitos", "python")).
		Build()
	c := interceptor.NewClient(base, interceptor.Funcs{
		Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if _, ok := obj.(*v1.Sandbox); ok {
				creates.Add(1)
				return errors.New("apiserver unavailable")
			}
			return cl.Create(ctx, obj, opts...)
		},
	})
	cp := New(c,
		WithPollInterval(5*time.Millisecond), WithReadyTimeout(50*time.Millisecond),
		WithSingleTenantNamespace("mitos"),
		WithCheckout(CheckoutConfig{Pools: []string{"python"}, Floor: 1, Cap: 2, MaxAge: 10 * time.Minute}))

	// The keepalive is exercised by the checkout_keepalive tests; here it
	// must not dial the fake endpoints, so warming is a no-op.
	cp.checkout.warm = noopWarm
	cp.checkout.reconcilePool(context.Background(), "python")
	if n := creates.Load(); n != 1 {
		t.Fatalf("first pass made %d create attempts, want 1", n)
	}
	cp.checkout.reconcilePool(context.Background(), "python")
	if n := creates.Load(); n != 1 {
		t.Fatalf("pass during backoff made a create attempt (total %d), want none", n)
	}
}

// TestReconcileAdoptsExistingBufferedSandboxes asserts a replica whose memory
// is empty (a restart, or the other replica refilled) rebuilds a claimable
// entry from the cluster: endpoint and pod from status, token from the
// controller-owned Secret.
func TestReconcileAdoptsExistingBufferedSandboxes(t *testing.T) {
	c := newFakeClient(t, poolInNs("mitos", "python"))
	cp := New(c,
		WithPollInterval(5*time.Millisecond), WithReadyTimeout(2*time.Second),
		WithSingleTenantNamespace("mitos"),
		WithCheckout(CheckoutConfig{Pools: []string{"python"}, Floor: 1, Cap: 4, MaxAge: 10 * time.Minute}))

	e := seedBuffered(t, c, "sb-adopt")
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "sb-adopt" + tokenSecretSuffix, Namespace: "mitos"},
		Data:       map[string][]byte{"token": []byte("tok-sb-adopt"), "endpoint": []byte(e.endpoint)},
	}
	if err := c.Create(context.Background(), sec); err != nil {
		t.Fatalf("seed secret: %v", err)
	}

	// The keepalive is exercised by the checkout_keepalive tests; here it
	// must not dial the fake endpoints, so warming is a no-op.
	cp.checkout.warm = noopWarm
	cp.checkout.reconcilePool(context.Background(), "python")

	got, ok := cp.checkout.pop("python")
	if !ok {
		t.Fatal("nothing adopted")
	}
	if got.name != "sb-adopt" || got.token != "tok-sb-adopt" || got.endpoint != e.endpoint || got.resourceVersion == "" {
		t.Fatalf("adopted entry incomplete: %+v", got)
	}
}

// TestReconcilePrunesFailedBufferedSandboxes asserts a buffered CR that is
// not Ready and not merely in flight (Failed) is deleted and never cached.
func TestReconcilePrunesFailedBufferedSandboxes(t *testing.T) {
	c := newFakeClient(t, poolInNs("mitos", "python"))
	cp := New(c,
		WithPollInterval(5*time.Millisecond), WithReadyTimeout(2*time.Second),
		WithSingleTenantNamespace("mitos"),
		WithCheckout(CheckoutConfig{Pools: []string{"python"}, Floor: 0, Cap: 4, MaxAge: 10 * time.Minute}))
	// Floor 0 keeps this pass from refilling, isolating the prune assertion.
	cp.checkout.cfg.Floor = 0

	fail := &v1.Sandbox{ObjectMeta: metav1.ObjectMeta{
		Name: "sb-failed", Namespace: "mitos",
		Labels: map[string]string{BufferedLabelKey: "true", bufferedPoolLabelKey: "python"},
	}, Spec: v1.SandboxSpec{Source: v1.SandboxSource{PoolRef: &v1.LocalObjectReference{Name: "python"}}}}
	if err := c.Create(context.Background(), fail); err != nil {
		t.Fatalf("seed: %v", err)
	}
	fail.Status.Phase = v1.SandboxFailed
	if err := c.Status().Update(context.Background(), fail); err != nil {
		t.Fatalf("status: %v", err)
	}

	// The keepalive is exercised by the checkout_keepalive tests; here it
	// must not dial the fake endpoints, so warming is a no-op.
	cp.checkout.warm = noopWarm
	cp.checkout.reconcilePool(context.Background(), "python")

	var gone v1.Sandbox
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "mitos", Name: "sb-failed"}, &gone); !apierrors.IsNotFound(err) {
		t.Fatalf("failed buffered sandbox not deleted: err=%v", err)
	}
	if _, ok := cp.checkout.pop("python"); ok {
		t.Fatal("a failed buffered sandbox was cached as claimable")
	}
}

// TestReconcileReapsOverAgeBufferedSandboxes asserts a buffered CR older than
// MaxAge is recycled (deleted and dropped), bounding how stale a handed-out
// sandbox can be.
func TestReconcileReapsOverAgeBufferedSandboxes(t *testing.T) {
	c := newFakeClient(t, poolInNs("mitos", "python"))
	cp := New(c,
		WithPollInterval(5*time.Millisecond), WithReadyTimeout(2*time.Second),
		WithSingleTenantNamespace("mitos"),
		WithCheckout(CheckoutConfig{Pools: []string{"python"}, Floor: 1, Cap: 4, MaxAge: 10 * time.Minute}))
	cp.checkout.cfg.Floor = 0 // no refill noise

	e := seedBuffered(t, c, "sb-old")
	cp.checkout.add(e)
	// Age is CreationTimestamp-based; step the control plane clock past MaxAge.
	cp.now = func() time.Time { return time.Now().Add(11 * time.Minute) }

	// The keepalive is exercised by the checkout_keepalive tests; here it
	// must not dial the fake endpoints, so warming is a no-op.
	cp.checkout.warm = noopWarm
	cp.checkout.reconcilePool(context.Background(), "python")

	var gone v1.Sandbox
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "mitos", Name: "sb-old"}, &gone); !apierrors.IsNotFound(err) {
		t.Fatalf("over-age buffered sandbox not reaped: err=%v", err)
	}
	if _, ok := cp.checkout.pop("python"); ok {
		t.Fatal("an over-age buffered sandbox stayed claimable")
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
