package quota

import (
	"context"
	"sync"
	"time"
)

// DefaultSuspensionCacheTTL is the read-cache lifetime the gateway uses over the
// durable suspension store. A few seconds keeps the hot request path off
// Postgres while bounding how stale a replica's view can be.
const DefaultSuspensionCacheTTL = 3 * time.Second

// CachedSuspensionStore wraps a SuspensionStore with a short-TTL in-process read
// cache for IsSuspended, the call that sits on the gateway's per-request hot
// path. It caches BOTH outcomes (suspended and not-suspended; the overwhelmingly
// common case is "not suspended", so negative caching is what actually keeps
// Postgres off the hot path).
//
// STALENESS BOUND: a suspension or lift written by ANOTHER replica takes effect
// on this replica within the TTL; that bounded convergence is the entire point
// of the durable store (issue #615), replacing the unbounded divergence of the
// per-replica in-memory store. Writes THROUGH this wrapper (Suspend, Lift)
// invalidate the org's cache entry, so the writing replica observes its own
// write immediately.
//
// MEMORY BOUND: the map holds at most one small entry per org id ever queried,
// and the org id comes from the gateway-verified key, so an unauthenticated
// caller cannot grow it; expired entries are overwritten in place on the next
// read for that org.
//
// ERROR POSTURE: an inner-store read error is returned to the caller and is
// never cached; the enforcer treats a suspension-store error as a DENY (fail
// closed), and a stale cached "not suspended" is never served in place of an
// error, so a Postgres outage cannot reopen the door for a possibly-suspended
// org beyond entries already inside their TTL.
type CachedSuspensionStore struct {
	inner SuspensionStore
	ttl   time.Duration
	now   func() time.Time

	mu      sync.RWMutex
	entries map[string]suspensionCacheEntry
}

// suspensionCacheEntry is one cached IsSuspended result with its expiry.
type suspensionCacheEntry struct {
	sus       Suspension
	suspended bool
	expires   time.Time
}

// NewCachedSuspensionStore wraps inner with a TTL read cache. A non-positive ttl
// defaults to DefaultSuspensionCacheTTL; a nil clock defaults to time.Now.
func NewCachedSuspensionStore(inner SuspensionStore, ttl time.Duration, now func() time.Time) *CachedSuspensionStore {
	if ttl <= 0 {
		ttl = DefaultSuspensionCacheTTL
	}
	if now == nil {
		now = time.Now
	}
	return &CachedSuspensionStore{
		inner:   inner,
		ttl:     ttl,
		now:     now,
		entries: map[string]suspensionCacheEntry{},
	}
}

// compile-time assertion that the wrapper satisfies the contract.
var _ SuspensionStore = (*CachedSuspensionStore)(nil)

// Suspend writes through to the inner store and invalidates the org's cache
// entry so this replica observes the suspension immediately.
func (c *CachedSuspensionStore) Suspend(ctx context.Context, sus Suspension) error {
	if err := c.inner.Suspend(ctx, sus); err != nil {
		return err
	}
	c.invalidate(sus.OrgID)
	return nil
}

// Lift writes through to the inner store and invalidates the org's cache entry
// so this replica observes the lift immediately.
func (c *CachedSuspensionStore) Lift(ctx context.Context, orgID string) (bool, error) {
	ok, err := c.inner.Lift(ctx, orgID)
	if err != nil {
		return ok, err
	}
	c.invalidate(orgID)
	return ok, nil
}

// IsSuspended serves from the cache within the TTL and reads through otherwise.
// An inner error is returned uncached (fail closed at the enforcer).
func (c *CachedSuspensionStore) IsSuspended(ctx context.Context, orgID string) (Suspension, bool, error) {
	now := c.now()
	c.mu.RLock()
	e, ok := c.entries[orgID]
	c.mu.RUnlock()
	if ok && now.Before(e.expires) {
		return e.sus, e.suspended, nil
	}

	sus, suspended, err := c.inner.IsSuspended(ctx, orgID)
	if err != nil {
		return Suspension{}, false, err
	}
	c.mu.Lock()
	c.entries[orgID] = suspensionCacheEntry{sus: sus, suspended: suspended, expires: now.Add(c.ttl)}
	c.mu.Unlock()
	return sus, suspended, nil
}

// invalidate drops the org's cache entry.
func (c *CachedSuspensionStore) invalidate(orgID string) {
	c.mu.Lock()
	delete(c.entries, orgID)
	c.mu.Unlock()
}
