package controlplane

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "mitos.run/mitos/api/v1"
	"mitos.run/mitos/internal/saas"
	"mitos.run/mitos/internal/tenant"
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

func (c CheckoutConfig) enabledFor(pool string) bool {
	for _, p := range c.Pools {
		if p == pool {
			return true
		}
	}
	return false
}

// bufferedSandbox is one cached, already-Ready, org-less sandbox. The token
// value stays in memory and the controller-owned Secret only, exactly the
// custody chain a classic create has; it is never logged.
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
	// nextRefill backs off refill attempts per pool after failures (the #894
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
// lost to the other replica or already reaped). On success the CR already
// carries the org labels, so runtime authz (getOwned) is satisfied before
// the caller's first exec can arrive.
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
		// The same telemetry contract as readyResponse: the gateway attaches
		// the non-identifying pool name to sandbox.created events.
		resp.Header.Set("X-Mitos-Pool", e.pool)
		return resp, true
	}
}

// stampOrg is THE checkout write: one resourceVersion-guarded patch that
// atomically drops the buffered label and stamps the org labels, making it
// both the mutual exclusion between gateway replicas and the claim-time org
// attribution. A conflict or a vanished CR means this entry is not ours (the
// other replica won, or a reaper got it): report false so claim tries the
// next entry.
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
		if backoff <= 0 || backoff > refillBackoffCap {
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
// (watch-before-create and all): buffered + pool labels, NO org labels, so it
// bills nobody and matches no caller until checkout.
func (b *checkoutBuffer) refillOne(ctx context.Context, pool string) error {
	ns := b.k.singleTenantNamespace
	sb := &v1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      generateName(),
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
