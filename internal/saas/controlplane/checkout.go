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
