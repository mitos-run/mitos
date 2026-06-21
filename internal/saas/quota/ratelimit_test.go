package quota

import (
	"testing"
	"time"
)

// TestTokenBucketAllowsUnderRate asserts a fresh bucket allows up to its burst
// capacity without refill.
func TestTokenBucketAllowsUnderRate(t *testing.T) {
	b := newBucket(60, 5) // 60/min, burst 5
	now := time.Unix(0, 0)
	for i := 0; i < 5; i++ {
		if !b.allow(now, 1) {
			t.Fatalf("request %d denied within burst capacity", i)
		}
	}
}

// TestTokenBucketRejectsOverRate asserts the bucket denies once the burst is
// spent and no time has passed to refill.
func TestTokenBucketRejectsOverRate(t *testing.T) {
	b := newBucket(60, 5)
	now := time.Unix(0, 0)
	for i := 0; i < 5; i++ {
		b.allow(now, 1)
	}
	if b.allow(now, 1) {
		t.Fatal("request beyond burst with no refill must be denied")
	}
}

// TestTokenBucketRefillsOverTime asserts tokens replenish at the configured rate:
// after the burst is spent, waiting one full minute restores the full burst.
func TestTokenBucketRefillsOverTime(t *testing.T) {
	b := newBucket(60, 5) // 60 tokens/min => 1 token/sec
	now := time.Unix(0, 0)
	for i := 0; i < 5; i++ {
		b.allow(now, 1)
	}
	if b.allow(now, 1) {
		t.Fatal("precondition: bucket should be empty")
	}
	// One second restores one token (60/min).
	now = now.Add(time.Second)
	if !b.allow(now, 1) {
		t.Fatal("one token should have refilled after one second")
	}
	if b.allow(now, 1) {
		t.Fatal("only one token should refill in one second")
	}
	// A full minute restores the full burst (capped at capacity, not above).
	now = now.Add(time.Minute)
	allowed := 0
	for i := 0; i < 100; i++ {
		if b.allow(now, 1) {
			allowed++
		}
	}
	if allowed != 5 {
		t.Fatalf("refill capped at burst: allowed %d after a full minute, want 5", allowed)
	}
}

// TestRateLimiterIsolatesKeys asserts per-key isolation: exhausting org A's
// bucket does not deny org B (and per-IP buckets are independent of per-org).
func TestRateLimiterIsolatesKeys(t *testing.T) {
	now := time.Unix(0, 0)
	rl := NewRateLimiter(func() time.Time { return now })
	// Drain org A's lifecycle bucket.
	for i := 0; i < 100; i++ {
		rl.Allow("org-a", BucketLifecycle, 60, 5)
	}
	if rl.Allow("org-a", BucketLifecycle, 60, 5) {
		t.Fatal("org-a lifecycle bucket should be drained")
	}
	// org-b is independent.
	if !rl.Allow("org-b", BucketLifecycle, 60, 5) {
		t.Fatal("org-b must not be affected by org-a's drained bucket")
	}
	// org-a's in-sandbox bucket is a different bucket than its lifecycle bucket.
	if !rl.Allow("org-a", BucketInSandbox, 60, 5) {
		t.Fatal("org-a in-sandbox bucket must be independent of its lifecycle bucket")
	}
}

// TestRateLimiterRefillsAcrossCalls asserts the limiter refills a key's bucket as
// wall-clock advances between Allow calls.
func TestRateLimiterRefillsAcrossCalls(t *testing.T) {
	now := time.Unix(0, 0)
	rl := NewRateLimiter(func() time.Time { return now })
	for i := 0; i < 5; i++ {
		rl.Allow("org", BucketLifecycle, 60, 5)
	}
	if rl.Allow("org", BucketLifecycle, 60, 5) {
		t.Fatal("bucket should be drained")
	}
	now = now.Add(time.Minute)
	if !rl.Allow("org", BucketLifecycle, 60, 5) {
		t.Fatal("bucket should have refilled after a minute")
	}
}
