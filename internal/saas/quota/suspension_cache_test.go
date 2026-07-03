package quota

import (
	"context"
	"errors"
	"testing"
	"time"
)

// countingSuspensionStore wraps MemSuspensionStore and counts IsSuspended reads
// so the cache tests can assert whether a read hit the inner store. A settable
// error simulates an unreachable durable store.
type countingSuspensionStore struct {
	inner   *MemSuspensionStore
	reads   int
	readErr error
}

func (s *countingSuspensionStore) Suspend(ctx context.Context, sus Suspension) error {
	return s.inner.Suspend(ctx, sus)
}

func (s *countingSuspensionStore) Lift(ctx context.Context, orgID string) (bool, error) {
	return s.inner.Lift(ctx, orgID)
}

func (s *countingSuspensionStore) IsSuspended(ctx context.Context, orgID string) (Suspension, bool, error) {
	s.reads++
	if s.readErr != nil {
		return Suspension{}, false, s.readErr
	}
	return s.inner.IsSuspended(ctx, orgID)
}

// cacheFixture builds a cached store over a counting inner store with a fake
// clock the test advances.
func cacheFixture(ttl time.Duration) (*CachedSuspensionStore, *countingSuspensionStore, *time.Time) {
	inner := &countingSuspensionStore{inner: NewMemSuspensionStore()}
	now := time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)
	c := NewCachedSuspensionStore(inner, ttl, func() time.Time { return now })
	return c, inner, &now
}

// TestCachedSuspensionStoreServesFromCacheWithinTTL asserts that within the TTL
// a repeat IsSuspended (for both a suspended and a not-suspended org) is served
// from the cache, never re-reading the inner store; that negative caching is the
// hot-path property the gateway relies on.
func TestCachedSuspensionStoreServesFromCacheWithinTTL(t *testing.T) {
	c, inner, _ := cacheFixture(3 * time.Second)
	ctx := context.Background()

	// Not-suspended org: first read hits the inner store, second is cached.
	if _, suspended, err := c.IsSuspended(ctx, "org-free"); err != nil || suspended {
		t.Fatalf("first read = suspended %v, err %v; want false, nil", suspended, err)
	}
	if _, suspended, err := c.IsSuspended(ctx, "org-free"); err != nil || suspended {
		t.Fatalf("second read = suspended %v, err %v; want false, nil", suspended, err)
	}
	if inner.reads != 1 {
		t.Fatalf("inner reads = %d, want 1 (second read must be served from cache)", inner.reads)
	}

	// Suspended org: the record round-trips from the cache too.
	if err := c.Suspend(ctx, Suspension{OrgID: "org-bad", Reason: ReasonAbuseSignal, Note: "egress spike"}); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	sus1, ok1, err := c.IsSuspended(ctx, "org-bad")
	if err != nil || !ok1 {
		t.Fatalf("suspended first read = ok %v, err %v; want true, nil", ok1, err)
	}
	before := inner.reads
	sus2, ok2, err := c.IsSuspended(ctx, "org-bad")
	if err != nil || !ok2 {
		t.Fatalf("suspended cached read = ok %v, err %v; want true, nil", ok2, err)
	}
	if inner.reads != before {
		t.Fatalf("inner reads grew on a cached read: %d -> %d", before, inner.reads)
	}
	if sus2 != sus1 {
		t.Fatalf("cached record = %+v, want %+v", sus2, sus1)
	}
}

// TestCachedSuspensionStoreExpiresAfterTTL asserts a cached entry expires: a
// suspension written directly to the inner store (simulating ANOTHER replica's
// write to the shared durable store) becomes visible after the TTL, never later.
// This is the documented staleness bound of issue #615.
func TestCachedSuspensionStoreExpiresAfterTTL(t *testing.T) {
	c, inner, now := cacheFixture(3 * time.Second)
	ctx := context.Background()

	// Prime the negative cache for the org.
	if _, suspended, _ := c.IsSuspended(ctx, "org-1"); suspended {
		t.Fatal("org-1 unexpectedly suspended")
	}

	// Another replica suspends the org in the shared store (bypasses this cache).
	if err := inner.Suspend(ctx, Suspension{OrgID: "org-1", Reason: ReasonManual, At: *now}); err != nil {
		t.Fatalf("inner suspend: %v", err)
	}

	// Within the TTL the stale negative entry is still served (the bound).
	if _, suspended, _ := c.IsSuspended(ctx, "org-1"); suspended {
		t.Fatal("suspension visible before TTL expiry; the cache is not caching")
	}

	// After the TTL the read goes through and sees the suspension.
	*now = now.Add(3*time.Second + time.Millisecond)
	if _, suspended, err := c.IsSuspended(ctx, "org-1"); err != nil || !suspended {
		t.Fatalf("after TTL: suspended %v, err %v; want true, nil", suspended, err)
	}
}

// TestCachedSuspensionStoreWriteThroughInvalidates asserts a Suspend or Lift
// through the wrapper takes effect immediately on this replica, with no TTL
// wait: the write invalidates the org's entry.
func TestCachedSuspensionStoreWriteThroughInvalidates(t *testing.T) {
	c, _, _ := cacheFixture(time.Hour) // a huge TTL: only invalidation can refresh.
	ctx := context.Background()

	// Prime the negative cache, then suspend through the wrapper.
	if _, suspended, _ := c.IsSuspended(ctx, "org-1"); suspended {
		t.Fatal("org-1 unexpectedly suspended")
	}
	if err := c.Suspend(ctx, Suspension{OrgID: "org-1", Reason: ReasonManual}); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	if _, suspended, err := c.IsSuspended(ctx, "org-1"); err != nil || !suspended {
		t.Fatalf("after write-through suspend: suspended %v, err %v; want true, nil", suspended, err)
	}

	// Lift through the wrapper: visible immediately too.
	if ok, err := c.Lift(ctx, "org-1"); err != nil || !ok {
		t.Fatalf("lift = %v, %v; want true, nil", ok, err)
	}
	if _, suspended, err := c.IsSuspended(ctx, "org-1"); err != nil || suspended {
		t.Fatalf("after write-through lift: suspended %v, err %v; want false, nil", suspended, err)
	}
}

// TestCachedSuspensionStoreErrorIsNotCached asserts an inner-store error is
// propagated (the enforcer maps it to a deny, fail closed) and is NOT cached: a
// recovered store serves the truth on the next read, and a pre-error cached
// entry within its TTL is still served (bounded, not unbounded, staleness).
func TestCachedSuspensionStoreErrorIsNotCached(t *testing.T) {
	c, inner, _ := cacheFixture(3 * time.Second)
	ctx := context.Background()
	boom := errors.New("postgres unreachable")

	// An error on a cold entry propagates.
	inner.readErr = boom
	if _, _, err := c.IsSuspended(ctx, "org-1"); !errors.Is(err, boom) {
		t.Fatalf("cold read during outage: err = %v, want the store error (fail closed)", err)
	}

	// Recovery: the next read goes through (the error was not cached as a value).
	inner.readErr = nil
	if _, suspended, err := c.IsSuspended(ctx, "org-1"); err != nil || suspended {
		t.Fatalf("read after recovery: suspended %v, err %v; want false, nil", suspended, err)
	}

	// A cached entry within its TTL is served even during an outage (bounded
	// staleness, not a new query per request).
	inner.readErr = boom
	if _, suspended, err := c.IsSuspended(ctx, "org-1"); err != nil || suspended {
		t.Fatalf("cached read during outage: suspended %v, err %v; want false, nil", suspended, err)
	}
}
