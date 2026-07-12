# Pre-claimed Checkout Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** The hosted gateway serves eligible creates from a buffer of already-activated sandboxes, so the hot path is one label patch instead of the whole claim round trip.

**Architecture:** Buffered sandboxes are real Sandbox CRs in the single-tenant namespace carrying `mitos.run/buffered: "true"` and NO org labels. Checkout is one resourceVersion-guarded patch that removes the buffered label and stamps the org labels (exclusivity + attribution in one write). A per-replica reconcile loop refills to a floor, adopts after restarts, and reaps stale entries. The controller propagates the CR org label to the backing husk pod (the trusted billing label) on the reconcile the checkout patch triggers.

**Tech Stack:** Go, controller-runtime (fake client + interceptor in tests, envtest for the controller task), existing gateway create machinery from #895 (establishSandboxWatch/watchWait).

**Spec:** docs/superpowers/specs/2026-07-11-preclaimed-checkout-design.md

## Global Constraints

- No em or en dashes anywhere; conventional commits; every commit `git commit -s` (DCO).
- Secret VALUES (tokens) never logged, never in errors or conditions.
- No public latency number claimed anywhere until bench/tti-latency.py reproduces it.
- Feature is default OFF (`checkout.pools` empty) and requires single-tenant namespace mode; per-org tenancy note lives in the spec.
- Lint gate is BOTH `~/go/bin/golangci-lint run --timeout=5m` and `GOOS=linux ~/go/bin/golangci-lint run --timeout=5m`.
- All controlplane tests live in package `controlplane` (internal test package) and reuse helpers from controlplane_test.go (newScheme, poolIn, flipToReadyWhenCreated, orgA/orgB) and readywatch/poolcheck test idioms.

---

### Task 1: Checkout config, buffer type, and the eligibility gate

**Files:**
- Create: `internal/saas/controlplane/checkout.go`
- Create: `internal/saas/controlplane/checkout_test.go`
- Modify: `internal/saas/controlplane/controlplane.go` (add fields + WithCheckout option)

**Interfaces:**
- Produces: `const BufferedLabelKey = "mitos.run/buffered"`, `const bufferedPoolLabelKey = "mitos.run/pool"`, `type CheckoutConfig struct { Pools []string; Floor, Cap int; MaxAge time.Duration }`, `type checkoutBuffer struct` with `func (b *checkoutBuffer) eligible(body createBody, pool string) bool`, `func newCheckoutBuffer(k *K8sControlPlane, cfg CheckoutConfig) *checkoutBuffer`, `type bufferedSandbox struct { name, pool, endpoint, token, resourceVersion, podName string; createdAt time.Time }`, K8sControlPlane field `checkout *checkoutBuffer`, `func WithCheckout(cfg CheckoutConfig) Option`.
- Consumes: `createBody` (forward.go), `K8sControlPlane.now`, `singleTenantNamespace`.

- [ ] **Step 1: Write the failing tests** (checkout_test.go)

```go
package controlplane

import (
	"testing"
	"time"
)

func checkoutCP(t *testing.T, objs ...client.Object) *K8sControlPlane {
	t.Helper()
	c := newFakeClient(t, objs...)
	cp := New(c,
		WithPollInterval(5*time.Millisecond),
		WithReadyTimeout(2*time.Second),
		WithSingleTenantNamespace("mitos"),
		WithCheckout(CheckoutConfig{Pools: []string{"python"}, Floor: 2, Cap: 4, MaxAge: 10 * time.Minute}),
	)
	return cp
}

// TestCheckoutEligibilityGate pins which creates may be served from the
// buffer: a checkout pool with NO env, secrets, workspace, fan-out, or
// lifetime knobs. Everything else must take the classic path.
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
```

Note: `createBody` fields must match forward.go exactly; if `Secrets` needs coverage add a case with `Secrets: []secretMountReq{{}}` after checking the struct.

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/saas/controlplane/ -run TestCheckout -v`
Expected: FAIL (undefined: CheckoutConfig, checkout field, WithCheckout)

- [ ] **Step 3: Implement** (checkout.go)

```go
package controlplane

import (
	"sync"
	"time"
)

// BufferedLabelKey marks a pre-created, org-less sandbox held by the gateway
// checkout buffer. Removing it, atomically with stamping the org labels, IS
// the checkout (see docs/superpowers/specs/2026-07-11-preclaimed-checkout-design.md).
const BufferedLabelKey = "mitos.run/buffered"

// bufferedPoolLabelKey carries the pool identity on a buffered Sandbox so the
// reconcile loop can LIST per pool (spec.source.poolRef is not selectable).
const bufferedPoolLabelKey = "mitos.run/pool"

// CheckoutConfig configures the pre-claimed checkout buffer. Pools empty
// means the feature is off.
type CheckoutConfig struct {
	Pools  []string
	Floor  int
	Cap    int
	MaxAge time.Duration
}

// bufferedSandbox is one cached, already-Ready, org-less sandbox.
type bufferedSandbox struct {
	name            string
	pool            string
	endpoint        string
	token           string
	resourceVersion string
	podName         string
	createdAt       time.Time
}

// checkoutBuffer serves eligible creates from pre-activated sandboxes. The
// cluster (label state on the CRs) is the source of truth; this struct is a
// cache plus the refill/janitor loop.
type checkoutBuffer struct {
	k   *K8sControlPlane
	cfg CheckoutConfig

	mu      sync.Mutex
	entries map[string][]bufferedSandbox
	// nextRefill backs off refill attempts per pool after failures (#894
	// shape: a refill that cannot succeed must not retry in a tight loop).
	nextRefill  map[string]time.Time
	refillFails map[string]int
}

func newCheckoutBuffer(k *K8sControlPlane, cfg CheckoutConfig) *checkoutBuffer {
	return &checkoutBuffer{
		k:           k,
		cfg:         cfg,
		entries:     make(map[string][]bufferedSandbox),
		nextRefill:  make(map[string]time.Time),
		refillFails: make(map[string]int),
	}
}

func (c CheckoutConfig) enabledFor(pool string) bool {
	for _, p := range c.Pools {
		if p == pool {
			return true
		}
	}
	return false
}

// eligible reports whether this create may be served from the buffer: a
// checkout-enabled pool and NOTHING tenant-specific that the activation
// handshake would have had to deliver (env, secrets, workspace), no fan-out,
// and no lifetime knobs (a buffered sandbox's TTL clock would predate the
// claim).
func (b *checkoutBuffer) eligible(body createBody, pool string) bool {
	if !b.cfg.enabledFor(pool) {
		return false
	}
	if len(body.Env) > 0 || len(body.Secrets) > 0 || body.Workspace != "" {
		return false
	}
	if body.Replicas > 1 {
		return false
	}
	if body.TTL != "" || body.Timeout != "" {
		return false
	}
	return true
}
```

In controlplane.go add to the struct (below poolSeen):

```go
	// checkout serves eligible creates from a buffer of pre-activated
	// sandboxes (nil = feature off). Constructed by New only when BOTH
	// WithCheckout and single-tenant mode are set; see the spec's migration
	// note for the per-org-namespace story.
	checkoutCfg *CheckoutConfig
	checkout    *checkoutBuffer
```

```go
// WithCheckout enables the pre-claimed checkout buffer for the named pools.
// Ignored (feature off) when Pools is empty or the control plane is not in
// single-tenant namespace mode. Zero Floor/Cap/MaxAge take defaults 2/4/10m.
func WithCheckout(cfg CheckoutConfig) Option {
	return func(k *K8sControlPlane) {
		if len(cfg.Pools) == 0 {
			return
		}
		if cfg.Floor <= 0 {
			cfg.Floor = 2
		}
		if cfg.Cap < cfg.Floor {
			cfg.Cap = cfg.Floor * 2
		}
		if cfg.MaxAge <= 0 {
			cfg.MaxAge = 10 * time.Minute
		}
		k.checkoutCfg = &cfg
	}
}
```

And in `New(...)` AFTER the options loop (find it at the bottom of controlplane.go; it applies opts then returns): construct the buffer only when both conditions hold:

```go
	if k.checkoutCfg != nil && k.singleTenantNamespace != "" {
		k.checkout = newCheckoutBuffer(k, *k.checkoutCfg)
	}
```

- [ ] **Step 4: Run to verify pass**

Run: `go test ./internal/saas/controlplane/ -run TestCheckout -v`
Expected: PASS (both tests)

- [ ] **Step 5: Commit**

```bash
git add internal/saas/controlplane/checkout.go internal/saas/controlplane/checkout_test.go internal/saas/controlplane/controlplane.go
git commit -s -m "feat(saas): checkout buffer scaffold, config, and the eligibility gate"
```

---

### Task 2: Extract createSandboxAndAwait from create() (pure refactor)

**Files:**
- Modify: `internal/saas/controlplane/forward.go`

**Interfaces:**
- Produces: `func (k *K8sControlPlane) createSandboxAndAwait(ctx context.Context, sb *v1.Sandbox, startedAt time.Time) (saas.ForwardResponse, error)` covering: pre-create watch establish, `k.c.Create`, error envelope mapping, watchWait, pollReady/pollReadyTicker fallbacks. Behavior byte-identical to today's create() tail.
- Consumes: establishSandboxWatch, watchWait, pollReady, pollReadyTicker (all landed in #895).

- [ ] **Step 1: Refactor.** In forward.go, move everything in `create()` from the `// Establish the ready watch BEFORE the Create` comment through the final `return k.pollReady(...)` into:

```go
// createSandboxAndAwait writes sb and blocks for its terminal outcome using
// the pre-established watch (one serialized round trip fewer, #895), failing
// open to the post-create watch or poll exactly as before. It is shared by
// the tenant create path and the checkout buffer's refill, which is why it
// takes a fully built Sandbox rather than a request body.
func (k *K8sControlPlane) createSandboxAndAwait(ctx context.Context, sb *v1.Sandbox, startedAt time.Time) (saas.ForwardResponse, error) {
	ns, name := sb.Namespace, sb.Name
	var wi watch.Interface
	if w, ok := k.c.(client.WithWatch); ok {
		watchCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		if established, werr := establishSandboxWatch(watchCtx, w, ns, name); werr == nil {
			wi = established
			defer wi.Stop()
		} else {
			slog.Warn("could not establish the sandbox ready watch before create; will wait post-create",
				"namespace", ns, "sandbox", name, "error", werr.Error())
		}
	}

	if err := k.c.Create(ctx, sb); err != nil {
		switch {
		case apierrors.IsAlreadyExists(err):
			return errResp(withStatus(apierr.Get(apierr.CodeInternal).
				WithCause("a sandbox with the generated name already exists; retry"), http.StatusConflict)), nil
		case isNamespaceMissing(err, ns):
			return errResp(namespaceMissingErr(sb.Labels[tenant.OrgLabelKey], ns)), nil
		case apierrors.IsInvalid(err), apierrors.IsBadRequest(err):
			return errResp(apierr.Get(apierr.CodeInvalidInput).
				WithCause("the api server rejected the sandbox object as invalid: " + err.Error())), nil
		default:
			return errResp(apierr.Get(apierr.CodeInternal).
				WithCause("could not create the sandbox object")), nil
		}
	}

	if wi != nil {
		deadline := k.now().Add(k.readyTimeout)
		if resp, done := k.watchWait(ctx, wi, ns, name, startedAt, deadline, "Pending"); done {
			return resp, nil
		}
		slog.Warn("sandbox ready watch closed before a terminal outcome; falling back to status polling",
			"namespace", ns, "sandbox", name)
		return k.pollReadyTicker(ctx, ns, name, startedAt, deadline)
	}
	return k.pollReady(ctx, ns, name, startedAt)
}
```

CAREFUL: `namespaceMissingErr(req.OrgID, ns)` in the original takes the org id; inside the helper use `sb.Labels[tenant.OrgLabelKey]` as shown (same value: create() sets `Labels: tenant.OrgLabels(req.OrgID)`). Verify `namespaceMissingErr`'s message renders acceptably with an empty org (the refill path); if it embeds the org id verbatim an empty string is fine.

create() then ends with:

```go
	return k.createSandboxAndAwait(ctx, sb, startedAt)
```

- [ ] **Step 2: Run the full package to verify the refactor is invisible**

Run: `go test ./internal/saas/controlplane/ -race`
Expected: ok (all existing tests, including the #895 watch tests, pass unchanged)

- [ ] **Step 3: Commit**

```bash
git add internal/saas/controlplane/forward.go
git commit -s -m "refactor(saas): extract createSandboxAndAwait for reuse by the checkout refill"
```

---

### Task 3: The claim path (pop, stamp, respond, fall back)

**Files:**
- Modify: `internal/saas/controlplane/checkout.go`
- Modify: `internal/saas/controlplane/forward.go` (create() consults the buffer)
- Modify: `internal/saas/controlplane/checkout_test.go`

**Interfaces:**
- Produces: `func (b *checkoutBuffer) claim(ctx context.Context, ns, org, pool string, startedAt time.Time) (saas.ForwardResponse, bool)`; `func (b *checkoutBuffer) pop(pool string) (bufferedSandbox, bool)`; `func (b *checkoutBuffer) add(e bufferedSandbox)`; `func (b *checkoutBuffer) stampOrg(ctx context.Context, ns, org string, e bufferedSandbox) bool`.
- Consumes: tenant.OrgLabels, jsonResp, BufferedLabelKey.

- [ ] **Step 1: Write the failing tests**

```go
// seedBuffered creates a Ready buffered sandbox CR (+ token Secret) in ns
// "mitos" and returns the entry the gateway would cache for it.
func seedBuffered(t *testing.T, c client.Client, name string) bufferedSandbox {
	t.Helper()
	sb := &v1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "mitos",
			Labels: map[string]string{BufferedLabelKey: "true", bufferedPoolLabelKey: "python"},
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

// TestCheckoutServesCreateFromBuffer asserts an eligible create is served
// from the buffer: 201 with the cached endpoint and token, NO Sandbox Create
// on the hot path, and the CR atomically loses the buffered label and gains
// the caller's org labels BEFORE the response returns.
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
		t.Errorf("org label = %q, want %q (runtime authz needs it before the first exec)", sb.Labels[tenant.OrgLabelKey], orgA)
	}
}

// TestCheckoutIsExclusiveAcrossReplicas asserts two gateways holding the same
// cached entry cannot both hand it out: the RV-guarded patch lets exactly one
// win, and the loser falls back to the classic path.
func TestCheckoutIsExclusiveAcrossReplicas(t *testing.T) {
	c := newFakeClient(t, poolIn2("mitos", "python"))
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
	c := newFakeClient(t, poolIn2("mitos", "python"))
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
```

Add the helper `poolIn2(ns, name string) *v1.SandboxPool` (namespace-explicit twin of poolIn) if controlplane_test.go does not already have one:

```go
func poolIn2(ns, name string) *v1.SandboxPool {
	return &v1.SandboxPool{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/saas/controlplane/ -run 'TestCheckoutServes|TestCheckoutIsExclusive|TestCheckoutEmptyBuffer' -v`
Expected: FAIL (undefined: add, claim; then behavioral failures)

- [ ] **Step 3: Implement** (checkout.go; imports grow to include context, log/slog, net/http, apierrors, metav1, client, v1, saas, tenant)

```go
// add caches one buffered sandbox (refill, adopt, tests).
func (b *checkoutBuffer) add(e bufferedSandbox) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.entries[e.pool] = append(b.entries[e.pool], e)
}

// pop removes and returns the oldest cached entry for pool.
func (b *checkoutBuffer) pop(pool string) (bufferedSandbox, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	q := b.entries[pool]
	if len(q) == 0 {
		return bufferedSandbox{}, false
	}
	e := q[0]
	b.entries[pool] = q[1:]
	return e, true
}

// claim serves one eligible create from the buffer. ok=false means the
// caller must take the classic path (empty buffer, or every candidate was
// lost to the other replica or reaped). On success the CR already carries
// the org labels: runtime authz (getOwned) is satisfied before the caller's
// first exec can arrive.
func (b *checkoutBuffer) claim(ctx context.Context, ns, org, pool string, startedAt time.Time) (saas.ForwardResponse, bool) {
	for {
		e, ok := b.pop(pool)
		if !ok {
			return saas.ForwardResponse{}, false
		}
		if !b.stampOrg(ctx, ns, org, e) {
			continue
		}
		payload := map[string]any{
			"id":           e.name,
			"endpoint":     e.endpoint,
			"token":        e.token,
			"phase":        string(v1.SandboxReady),
			"template_id":  e.pool,
			"fork_time_ms": float64(b.k.now().Sub(startedAt).Milliseconds()),
		}
		resp := jsonResp(http.StatusCreated, payload)
		resp.Header.Set("X-Mitos-Pool", e.pool)
		return resp, true
	}
}

// stampOrg is THE checkout write: one resourceVersion-guarded patch that
// atomically drops the buffered label and stamps the org labels. A conflict
// or a vanished CR means this entry is not ours (the other replica won, or
// the janitor or idle reaper got it): report false so claim tries the next.
func (b *checkoutBuffer) stampOrg(ctx context.Context, ns, org string, e bufferedSandbox) bool {
	old := &v1.Sandbox{ObjectMeta: metav1.ObjectMeta{
		Name:            e.name,
		Namespace:       ns,
		ResourceVersion: e.resourceVersion,
		Labels: map[string]string{
			BufferedLabelKey:     "true",
			bufferedPoolLabelKey: e.pool,
		},
	}}
	claimed := old.DeepCopy()
	claimed.Labels = tenant.OrgLabels(org)
	err := b.k.c.Patch(ctx, claimed, client.MergeFromWithOptions(old, client.MergeFromWithOptimisticLock{}))
	if err == nil {
		return true
	}
	if !apierrors.IsConflict(err) && !apierrors.IsNotFound(err) {
		slog.Warn("checkout stamp failed; entry dropped, create falls back",
			"sandbox", e.name, "error", err.Error())
	}
	return false
}
```

In forward.go create(), immediately AFTER the `pool == ""` rejection block and the replicas bound check, BEFORE the pool pre-check (a buffered entry proves the pool exists):

```go
	ns := k.namespaceForOrg(req.OrgID)

	// Pre-claimed checkout: an eligible create is served from the buffer of
	// already-activated sandboxes, paying ONE label patch instead of the
	// whole claim round trip. Any miss falls through to the classic path.
	if k.checkout != nil && k.checkout.eligible(body, pool) {
		if resp, ok := k.checkout.claim(ctx, ns, req.OrgID, pool, startedAt); ok {
			return resp, nil
		}
	}
```

(The existing `ns := k.namespaceForOrg(req.OrgID)` line moves up to serve both; do not declare it twice.)

- [ ] **Step 4: Run to verify pass, then the whole package**

Run: `go test ./internal/saas/controlplane/ -race`
Expected: ok

- [ ] **Step 5: Commit**

```bash
git add internal/saas/controlplane/checkout.go internal/saas/controlplane/checkout_test.go internal/saas/controlplane/forward.go
git commit -s -m "feat(saas): serve eligible creates from the checkout buffer with one attribution patch"
```

---

### Task 4: Refill with floor, cap, and failure backoff

**Files:**
- Modify: `internal/saas/controlplane/checkout.go`
- Modify: `internal/saas/controlplane/checkout_test.go`

**Interfaces:**
- Produces: `func (b *checkoutBuffer) reconcilePool(ctx context.Context, pool string)` (list, prune, adopt, reap, refill: Task 5 extends it; this task lands list-count + refill + backoff), `func (b *checkoutBuffer) refillOne(ctx context.Context, pool string) error`.
- Consumes: createSandboxAndAwait (Task 2), generateName (forward.go), readToken.

- [ ] **Step 1: Write the failing tests**

```go
// TestRefillFillsToFloorAndStopsAtIt asserts reconcilePool creates buffered
// sandboxes (org-less, buffered+pool labels) until the cluster count reaches
// the floor, one per pass, and a pass at the floor creates none.
func TestRefillFillsToFloorAndStopsAtIt(t *testing.T) {
	c := newFakeClient(t, poolIn2("mitos", "python"))
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
	// The refilled entries are cached and claimable.
	if _, ok := cp.checkout.pop("python"); !ok {
		t.Fatal("refill did not cache a claimable entry")
	}
}

// TestRefillBacksOffAfterFailure asserts a failed refill (no controller ever
// flips the sandbox Ready, so the create times out) sets a backoff: the next
// pass makes NO create attempt until the backoff expires.
func TestRefillBacksOffAfterFailure(t *testing.T) {
	var creates atomic.Int64
	base := fakeclient.NewClientBuilder().
		WithScheme(newScheme(t)).
		WithStatusSubresource(&v1.Sandbox{}).
		WithObjects(poolIn2("mitos", "python")).
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
		WithPollInterval(5*time.Millisecond), WithReadyTimeout(50*time.Millisecond),
		WithSingleTenantNamespace("mitos"),
		WithCheckout(CheckoutConfig{Pools: []string{"python"}, Floor: 1, Cap: 2, MaxAge: 10 * time.Minute}))

	cp.checkout.reconcilePool(context.Background(), "python")
	if n := creates.Load(); n != 1 {
		t.Fatalf("first pass made %d create attempts, want 1", n)
	}
	cp.checkout.reconcilePool(context.Background(), "python")
	if n := creates.Load(); n != 1 {
		t.Fatalf("pass during backoff made a create attempt (total %d), want none", n)
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/saas/controlplane/ -run TestRefill -v`
Expected: FAIL (undefined: reconcilePool)

- [ ] **Step 3: Implement** (checkout.go; add "encoding/json" and "fmt" imports)

```go
// refillBackoffBase and refillBackoffCap bound the retry cadence after
// consecutive refill failures (the #894 lesson: a refill that cannot succeed
// must never spin at full cadence).
const (
	refillBackoffBase = time.Second
	refillBackoffCap  = time.Minute
)

// reconcilePool is one pass of the buffer loop for one pool: count the
// cluster's buffered sandboxes and refill toward the floor, at most one
// create per pass so two replicas converge without a thundering herd.
// (Task 5 extends this pass with adopt, prune, and reap.)
func (b *checkoutBuffer) reconcilePool(ctx context.Context, pool string) {
	ns := b.k.singleTenantNamespace
	var list v1.SandboxList
	if err := b.k.c.List(ctx, &list, client.InNamespace(ns),
		client.MatchingLabels{BufferedLabelKey: "true", bufferedPoolLabelKey: pool}); err != nil {
		slog.Warn("checkout buffer list failed", "pool", pool, "error", err.Error())
		return
	}
	count := len(list.Items)
	if count >= b.cfg.Floor || count >= b.cfg.Cap {
		return
	}
	b.mu.Lock()
	wait := b.k.now().Before(b.nextRefill[pool])
	b.mu.Unlock()
	if wait {
		return
	}
	if err := b.refillOne(ctx, pool); err != nil {
		b.mu.Lock()
		b.refillFails[pool]++
		backoff := refillBackoffBase << (b.refillFails[pool] - 1)
		if backoff > refillBackoffCap {
			backoff = refillBackoffCap
		}
		b.nextRefill[pool] = b.k.now().Add(backoff)
		b.mu.Unlock()
		slog.Warn("checkout refill failed; backing off", "pool", pool, "error", err.Error())
		return
	}
	b.mu.Lock()
	b.refillFails[pool] = 0
	delete(b.nextRefill, pool)
	b.mu.Unlock()
}

// refillOne creates ONE buffered sandbox through the normal create machinery
// (watch-before-create and all): buffered + pool labels, NO org labels, so
// it bills nobody and matches no caller until checkout.
func (b *checkoutBuffer) refillOne(ctx context.Context, pool string) error {
	ns := b.k.singleTenantNamespace
	name := generateName()
	sb := &v1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			Labels:    map[string]string{BufferedLabelKey: "true", bufferedPoolLabelKey: pool},
		},
		Spec: v1.SandboxSpec{Source: v1.SandboxSource{PoolRef: &v1.LocalObjectReference{Name: pool}}},
	}
	resp, err := b.k.createSandboxAndAwait(ctx, sb, b.k.now())
	if err != nil {
		return err
	}
	if resp.Status != http.StatusCreated {
		return fmt.Errorf("buffered create for pool %q did not become ready (status %d)", pool, resp.Status)
	}
	var payload struct {
		ID       string `json:"id"`
		Endpoint string `json:"endpoint"`
		Token    string `json:"token"`
	}
	if err := json.Unmarshal(resp.Body, &payload); err != nil {
		return fmt.Errorf("parse buffered create payload: %w", err)
	}
	// One off-hot-path read to snapshot the resourceVersion (the checkout
	// patch's optimistic lock) and the backing pod name.
	var cur v1.Sandbox
	if err := b.k.c.Get(ctx, client.ObjectKey{Namespace: ns, Name: payload.ID}, &cur); err != nil {
		return fmt.Errorf("snapshot buffered sandbox: %w", err)
	}
	b.add(bufferedSandbox{
		name:            payload.ID,
		pool:            pool,
		endpoint:        payload.Endpoint,
		token:           payload.Token,
		resourceVersion: cur.ResourceVersion,
		podName:         cur.Status.Pod,
		createdAt:       cur.CreationTimestamp.Time,
	})
	return nil
}
```

NOTE: token values pass through memory exactly as in create(); never log them.

- [ ] **Step 4: Run to verify pass**

Run: `go test ./internal/saas/controlplane/ -run TestRefill -v && go test ./internal/saas/controlplane/ -race`
Expected: PASS, then ok

- [ ] **Step 5: Commit**

```bash
git add internal/saas/controlplane/checkout.go internal/saas/controlplane/checkout_test.go
git commit -s -m "feat(saas): checkout refill to a floor with capped backoff on failure"
```

---

### Task 5: Adopt, prune, reap (the janitor half of the pass) and the run loop

**Files:**
- Modify: `internal/saas/controlplane/checkout.go`
- Modify: `internal/saas/controlplane/checkout_test.go`

**Interfaces:**
- Produces: `func (k *K8sControlPlane) StartCheckout(ctx context.Context)` (no-op when checkout is nil; otherwise starts the loop goroutine), `func (b *checkoutBuffer) run(ctx context.Context)`; reconcilePool grows adopt/prune/reap.
- Consumes: readToken (forward.go).

- [ ] **Step 1: Write the failing tests**

```go
// TestReconcileAdoptsPrunesAndReaps asserts one pass: a Ready buffered CR
// unknown to this replica is ADOPTED (token from its Secret); a buffered CR
// past MaxAge is DELETED; a Failed buffered CR is DELETED; the memory cache
// ends up holding exactly the adoptable entry.
func TestReconcileAdoptsPrunesAndReaps(t *testing.T) {
	c := newFakeClient(t, poolIn2("mitos", "python"))
	cp := New(c,
		WithPollInterval(5*time.Millisecond), WithReadyTimeout(2*time.Second),
		WithSingleTenantNamespace("mitos"),
		WithCheckout(CheckoutConfig{Pools: []string{"python"}, Floor: 1, Cap: 4, MaxAge: 10 * time.Minute}))

	// Adoptable: Ready, in-age, with its token Secret.
	e := seedBuffered(t, c, "sb-adopt")
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "sb-adopt" + tokenSecretSuffix, Namespace: "mitos"},
		Data:       map[string][]byte{"token": []byte("tok-sb-adopt"), "endpoint": []byte(e.endpoint)},
	}
	if err := c.Create(context.Background(), sec); err != nil {
		t.Fatalf("seed secret: %v", err)
	}

	// Over-age: Ready but "created" long ago; age comes from CreationTimestamp,
	// which the fake client stamps at Create, so step the control plane clock
	// forward instead and exempt sb-adopt by re-seeding it after the step.
	_ = seedBuffered(t, c, "sb-old")
	cp.now = func() time.Time { return time.Now().Add(11 * time.Minute) }
	// Failed: buffered but not Ready.
	fail := &v1.Sandbox{ObjectMeta: metav1.ObjectMeta{
		Name: "sb-failed", Namespace: "mitos",
		Labels: map[string]string{BufferedLabelKey: "true", bufferedPoolLabelKey: "python"},
	}, Spec: v1.SandboxSpec{Source: v1.SandboxSource{PoolRef: &v1.LocalObjectReference{Name: "python"}}}}
	if err := c.Create(context.Background(), fail); err != nil {
		t.Fatalf("seed failed: %v", err)
	}
	failCur := &v1.Sandbox{}
	_ = c.Get(context.Background(), client.ObjectKey{Namespace: "mitos", Name: "sb-failed"}, failCur)
	failCur.Status.Phase = v1.SandboxFailed
	_ = c.Status().Update(context.Background(), failCur)

	// NOTE: with the clock stepped +11m, sb-adopt is over-age too. This test
	// wants exactly one survivor, so give the adoptable entry a fresh clock:
	// run the pass with MaxAge large enough to keep sb-adopt (20m) but a
	// separate assertion pass for the reap: adjust cfg inline.
	cp.checkout.cfg.MaxAge = 20 * time.Minute
	stop := flipToReadyWhenCreatedInNs(t, c, "mitos", "10.0.0.5:9091", "tok-refill")
	cp.checkout.reconcilePool(context.Background(), "python")
	stop()

	// sb-failed pruned+deleted; sb-adopt adopted; sb-old still in-age at 20m.
	var gone v1.Sandbox
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "mitos", Name: "sb-failed"}, &gone); !apierrors.IsNotFound(err) {
		t.Fatalf("failed buffered sandbox not deleted: err=%v", err)
	}
	popped, ok := cp.checkout.pop("python")
	if !ok {
		t.Fatal("nothing adopted")
	}
	if popped.name != "sb-adopt" && popped.name != "sb-old" {
		t.Fatalf("adopted %q, want a seeded buffered sandbox", popped.name)
	}
	if popped.token == "" {
		t.Fatal("adopted entry has no token (Secret read failed)")
	}

	// Now the reap: shrink MaxAge back to 10m; the pass must delete over-age CRs.
	cp.checkout.cfg.MaxAge = 10 * time.Minute
	cp.checkout.reconcilePool(context.Background(), "python") // reap pass; refill may create anew, that is fine
	var old v1.Sandbox
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "mitos", Name: "sb-old"}, &old); !apierrors.IsNotFound(err) {
		t.Fatalf("over-age buffered sandbox not reaped: err=%v", err)
	}
}
```

If the interleaving above proves brittle when run, split it into three focused tests (adopt / prune-failed / reap-over-age), each seeding only its own CR; keep the same assertions.

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/saas/controlplane/ -run TestReconcileAdopts -v`
Expected: FAIL (adopt/prune/reap not implemented; pop returns nothing or the failed CR survives)

- [ ] **Step 3: Implement.** Extend reconcilePool: after the LIST, before the refill decision:

```go
	// Sync the cache against the cluster: the CRs are the truth.
	known := make(map[string]bool)
	b.mu.Lock()
	for _, e := range b.entries[pool] {
		known[e.name] = true
	}
	b.mu.Unlock()

	live := 0
	for i := range list.Items {
		sb := &list.Items[i]
		age := b.k.now().Sub(sb.CreationTimestamp.Time)
		switch {
		case age > b.cfg.MaxAge:
			// Bounded staleness: a buffered VM runs live while it waits, so
			// entries past MaxAge are recycled, not handed to a tenant.
			b.dropAndDelete(ctx, pool, sb)
		case sb.Status.Phase == v1.SandboxFailed,
			sb.Status.Phase == v1.SandboxReady && sb.Status.Endpoint == "":
			b.dropAndDelete(ctx, pool, sb)
		case sb.Status.Phase != v1.SandboxReady:
			// In flight (a refill mid-activation): neither adoptable nor
			// reapable yet; it counts toward the floor so we do not over-refill.
			live++
		default:
			live++
			if !known[sb.Name] {
				b.adopt(ctx, pool, sb)
			}
		}
	}
	count := live
```

(replace the earlier `count := len(list.Items)` from Task 4 with this `live` count) and add:

```go
// dropAndDelete removes sb from the cache and deletes the CR (terminate).
func (b *checkoutBuffer) dropAndDelete(ctx context.Context, pool string, sb *v1.Sandbox) {
	b.mu.Lock()
	q := b.entries[pool][:0]
	for _, e := range b.entries[pool] {
		if e.name != sb.Name {
			q = append(q, e)
		}
	}
	b.entries[pool] = q
	b.mu.Unlock()
	if err := b.k.c.Delete(ctx, sb); err != nil && !apierrors.IsNotFound(err) {
		slog.Warn("checkout reap failed", "sandbox", sb.Name, "error", err.Error())
	}
}

// adopt rebuilds a cache entry from the cluster after a gateway restart (or
// for a CR the other replica refilled): endpoint and pod from status, token
// from the controller-owned Secret.
func (b *checkoutBuffer) adopt(ctx context.Context, pool string, sb *v1.Sandbox) {
	token, err := b.k.readToken(ctx, sb.Namespace, sb.Name+tokenSecretSuffix)
	if err != nil {
		slog.Warn("checkout adopt could not read the token secret; leaving for the next pass",
			"sandbox", sb.Name, "error", err.Error())
		return
	}
	b.add(bufferedSandbox{
		name:            sb.Name,
		pool:            pool,
		endpoint:        sb.Status.Endpoint,
		token:           token,
		resourceVersion: sb.ResourceVersion,
		podName:         sb.Status.Pod,
		createdAt:       sb.CreationTimestamp.Time,
	})
}
```

And the loop plus its public entry point:

```go
// checkoutReconcileInterval paces the buffer loop; the hot path never waits
// on it (claim reads only the cache).
const checkoutReconcileInterval = 15 * time.Second

// StartCheckout starts the buffer's reconcile loop for the server's
// lifetime. A no-op when the checkout feature is off.
func (k *K8sControlPlane) StartCheckout(ctx context.Context) {
	if k.checkout == nil {
		return
	}
	go k.checkout.run(ctx)
}

func (b *checkoutBuffer) run(ctx context.Context) {
	t := time.NewTicker(checkoutReconcileInterval)
	defer t.Stop()
	for {
		for _, pool := range b.cfg.Pools {
			b.reconcilePool(ctx, pool)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
```

- [ ] **Step 4: Run to verify pass, whole package with race**

Run: `go test ./internal/saas/controlplane/ -race`
Expected: ok

- [ ] **Step 5: Commit**

```bash
git add internal/saas/controlplane/checkout.go internal/saas/controlplane/checkout_test.go
git commit -s -m "feat(saas): checkout adopt-on-restart, staleness reap, and the reconcile loop"
```

---

### Task 6: Controller propagates the CR org label to the husk pod (billing)

**Files:**
- Modify: `internal/controller/sandboxclaim_controller.go` (the Ready-claim block at ~line 413, inside `if claim.Status.Phase == v1.SandboxReady { if r.EnableHuskPods { ... } }`, right after the `reflectHuskBackingReadiness` block, where `lostPod` is in scope)
- Test: the existing envtest suite file that covers husk claims (grep `reflectHuskBackingReadiness` or `checkHuskPodLost` under `internal/controller/*_test.go` and add alongside)

**Interfaces:**
- Consumes: `checkHuskPodLost` already returns the backing pod (`lostPod`) even when healthy; `tenant.OrgLabelKey`.
- Produces: nothing new; behavior: a Ready husk claim whose CR org label differs from its pod's org label patches the pod label (idempotent).

**Why the controller and not the gateway:** the usage scraper bills the TRUSTED pod org label, and the trust chain is that the CONTROLLER writes pod labels. The gateway's checkout patch stamps the CR; the watch event it fires brings the reconciler here, which propagates. The gateway needs no pod RBAC at all.

- [ ] **Step 1: Write the failing envtest** (in the husk-claim envtest file; follow its existing setup helpers for creating a pool, a fake dormant pod, and driving a claim; the new test can also be written directly against a Ready claim + pod fixture)

```go
// TestReadyClaimPropagatesOrgLabelToHuskPod asserts claim-time org
// attribution reaches the backing pod: when a Ready claim's org label (set
// by the gateway checkout patch) differs from its husk pod's, one reconcile
// patches the pod. The usage scraper bills the pod label, so without this a
// checked-out sandbox meters nobody.
func TestReadyClaimPropagatesOrgLabelToHuskPod(t *testing.T) {
	// Fixture: a Ready claim with status.Pod set + the healthy backing pod
	// WITHOUT an org label; stamp tenant.OrgLabels("acme") on the claim;
	// run one Reconcile; assert pod.Labels[tenant.OrgLabelKey] == "acme".
	// Reuse the suite's existing husk fixtures for pool/pod/claim shape.
}
```

Fill the fixture with the suite's existing helper shapes (the file already builds Ready husk claims for the pod-lost tests; copy that fixture and add the org label to the claim only).

- [ ] **Step 2: Run to verify failure**

Run: `eval $(~/go/bin/setup-envtest use 1.31 -p env) && go test ./internal/controller/ -run TestReadyClaimPropagatesOrg -v`
Expected: FAIL (pod label never set)

- [ ] **Step 3: Implement.** In the Ready-claim block, after the `reflectHuskBackingReadiness` if-block and before the hydrate call:

```go
			// Claim-time org attribution (pre-claimed checkout): the gateway
			// stamps the org on a formerly buffered sandbox's CR labels; this
			// reconcile, triggered by that very patch, propagates the org to
			// the backing husk pod, whose TRUSTED label is what the usage
			// scraper bills. Idempotent, and a no-op for classic claims whose
			// pod was labeled at claim time.
			if lostPod != nil {
				if org := claim.Labels[tenant.OrgLabelKey]; org != "" && lostPod.Labels[tenant.OrgLabelKey] != org {
					podPatch := client.MergeFrom(lostPod.DeepCopy())
					if lostPod.Labels == nil {
						lostPod.Labels = map[string]string{}
					}
					lostPod.Labels[tenant.OrgLabelKey] = org
					if err := r.Patch(ctx, lostPod, podPatch); err != nil {
						return ctrl.Result{}, err
					}
				}
			}
```

Verify `checkHuskPodLost` indeed returns the pod when NOT lost (read its body first; if it returns nil for the healthy case, Get the pod by `claim.Status.Pod` instead, still inside the EnableHuskPods block).

- [ ] **Step 4: Run to verify pass, then the controller suite**

Run: `eval $(~/go/bin/setup-envtest use 1.31 -p env) && go test ./internal/controller/`
Expected: ok (whole suite)

- [ ] **Step 5: Commit**

```bash
git add internal/controller/sandboxclaim_controller.go internal/controller/<the test file>
git commit -s -m "feat(controller): propagate claim org label to the husk pod for checkout billing"
```

---

### Task 7: Gateway wiring (flags, startup, logging)

**Files:**
- Modify: `cmd/gateway/main.go` (flags near line 107, newControlPlane near line 78, startup logging near line 163)

**Interfaces:**
- Consumes: `controlplane.WithCheckout`, `controlplane.CheckoutConfig`, `StartCheckout` (real control plane only; the mock path never starts it).

- [ ] **Step 1: Add flags** (following the exact style of the existing flag block: flag.String with os.Getenv defaults)

```go
	checkoutPools := flag.String("checkout-pools", os.Getenv("MITOS_GATEWAY_CHECKOUT_POOLS"), "comma-separated pool names served by the pre-claimed checkout buffer (empty disables the feature); requires --single-tenant-namespace")
	checkoutFloor := flag.Int("checkout-floor", envInt("MITOS_GATEWAY_CHECKOUT_FLOOR", 2), "buffered sandboxes to keep ready per checkout pool")
	checkoutCap := flag.Int("checkout-cap", envInt("MITOS_GATEWAY_CHECKOUT_CAP", 4), "hard ceiling of buffered sandboxes per checkout pool")
	checkoutMaxAge := flag.Duration("checkout-max-age", envDuration("MITOS_GATEWAY_CHECKOUT_MAX_AGE", 10*time.Minute), "buffered sandboxes older than this are recycled")
```

If `envInt`/`envDuration` helpers do not exist in main.go, add them (strconv.Atoi / time.ParseDuration with the default on any error). Thread the values through `newControlPlane` (extend its signature) into:

```go
	if pools := splitNonEmpty(*checkoutPools); len(pools) > 0 {
		opts = append(opts, controlplane.WithCheckout(controlplane.CheckoutConfig{
			Pools: pools, Floor: *checkoutFloor, Cap: *checkoutCap, MaxAge: *checkoutMaxAge,
		}))
	}
```

with `splitNonEmpty` = strings.Split on "," with TrimSpace, dropping empties. After the real control plane is constructed and the server context exists, start the loop and log the state honestly:

```go
		real.(*controlplane.K8sControlPlane).StartCheckout(ctx)
```

(newControlPlane returns saas.ControlPlane; either return the concrete type alongside, or add StartCheckout to nothing else and type-assert as shown, logging and skipping if the assertion fails.) Log at startup: enabled pools + floor/cap/maxAge when on; a WARNING when --checkout-pools is set without --single-tenant-namespace (the buffer silently stays off otherwise, which must not be silent).

- [ ] **Step 2: Build and run the gateway's own tests**

Run: `go build ./cmd/gateway/ && go test ./cmd/gateway/...`
Expected: builds; existing tests ok

- [ ] **Step 3: Commit**

```bash
git add cmd/gateway/main.go
git commit -s -m "feat(saas): gateway flags and startup wiring for the pre-claimed checkout"
```

---

### Task 8: Chart values, schema, RBAC patch verb, threat model, spec amendments

**Files:**
- Modify: `deploy/charts/mitos/values.yaml` (gateway block, after `enforce`)
- Modify: `deploy/charts/mitos/values.schema.json` (gateway properties; the schema rejects unknown keys, #667, so this is REQUIRED for the release to install)
- Modify: `deploy/charts/mitos/templates/gateway.yaml` (args)
- Modify: `deploy/charts/mitos/templates/gateway-rbac.yaml` (add `patch` to the sandboxes verbs, with a comment naming the checkout attribution patch as the reason)
- Modify: `docs/threat-model.md` (one row: org-less buffered Ready sandboxes; unreachable through the gateway pre-checkout because getOwned matches no org; token custody window extended to maxAge; no tenant data in the VM by the eligibility gate)
- Modify: `docs/superpowers/specs/2026-07-11-preclaimed-checkout-design.md` (three amendments: idle-reap needs NO controller exemption because a reaped buffered CR is self-healing via NotFound-tolerant pop; pod org label propagation is CONTROLLER-side; e2e proof is unit + prod acceptance, no kind gateway job)

values.yaml:

```yaml
  # Pre-claimed checkout: keep a small buffer of already-activated sandboxes
  # per listed pool and serve eligible creates (no env, secrets, workspace,
  # fan-out, or TTL) from it with a single attribution patch. Requires
  # single-tenant namespace mode; buffered sandboxes carry no org and bill
  # nobody until claimed (platform cost, bounded by cap and maxAge).
  checkout:
    pools: []
    floor: 2
    cap: 4
    maxAge: 10m
```

gateway.yaml args (inside the existing args list, following the conditional style used by other optional args):

```yaml
          {{- if .Values.gateway.checkout.pools }}
          - --checkout-pools={{ join "," .Values.gateway.checkout.pools }}
          - --checkout-floor={{ .Values.gateway.checkout.floor }}
          - --checkout-cap={{ .Values.gateway.checkout.cap }}
          - --checkout-max-age={{ .Values.gateway.checkout.maxAge }}
          {{- end }}
```

values.schema.json, inside the gateway properties object:

```json
"checkout": {
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "pools": {"type": "array", "items": {"type": "string"}},
    "floor": {"type": "integer", "minimum": 1},
    "cap": {"type": "integer", "minimum": 1},
    "maxAge": {"type": "string"}
  }
}
```

- [ ] **Step 1: Make all six edits.**
- [ ] **Step 2: Verify the chart renders both ways**

Run: `helm template mitos deploy/charts/mitos --kube-version 1.31.0 > /dev/null && helm template mitos deploy/charts/mitos --kube-version 1.31.0 --set gateway.checkout.pools='{python}' | grep -A2 checkout-pools`
Expected: clean render; the second shows the four args

- [ ] **Step 3: Commit**

```bash
git add deploy/charts/mitos docs/threat-model.md docs/superpowers/specs/2026-07-11-preclaimed-checkout-design.md
git commit -s -m "feat(chart): gateway.checkout values, RBAC patch verb, and the threat-model row"
```

---

### Task 9: Full verification and the PR

- [ ] **Step 1: Whole-repo gates**

Run, each must pass:
```bash
go build ./...
go test ./internal/saas/... -race
make test-unit
eval $(~/go/bin/setup-envtest use 1.31 -p env) && go test ./internal/controller/
~/go/bin/golangci-lint run --timeout=5m
GOOS=linux ~/go/bin/golangci-lint run --timeout=5m
```

- [ ] **Step 2: Commit the spec + plan** (if not yet on the branch), push, open the PR with the repo template (Thinking Path, Verification, Model Used; conventional title `feat(saas): pre-claimed checkout, the gateway hands out an already-activated sandbox`; no em/en dashes; Related: the TTI issue #871 and spec/plan paths). State explicitly: default OFF, no public number claimed, prod acceptance = bench/tti-latency.py after a roll with `gateway.checkout.pools={python}`.
- [ ] **Step 3: Drive CI green, resolve every CodeRabbit comment, auto-merge on green.**

---

## Self-Review Notes

- Spec coverage: eligibility (T1), one-patch checkout + exclusivity + fallback (T3), refill floor/cap/backoff (T4), adopt/prune/reap + loop (T5), controller pod-label propagation replacing the spec's async-gateway-patch (T6 + spec amendment in T8), config/Helm/RBAC/threat model (T7/T8), honesty + acceptance (T9). The spec's "janitor re-patches org-less pods" sweep is superseded by T6 (the reconciler IS the sweep, watch-triggered); amended in T8.
- The fake client's Watch + interceptor idioms match the #895 tests; the exclusivity test relies on the fake client honoring resourceVersion on Patch with MergeFromWithOptimisticLock (it does; it 409s on mismatch).
- Type consistency: bufferedSandbox fields, CheckoutConfig fields, and label constants are used identically across T1-T5; tenant.OrgLabelKey/OrgLabels shared with T6.
