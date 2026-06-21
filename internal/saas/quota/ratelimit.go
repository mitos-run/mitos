package quota

import (
	"sync"
	"time"
)

// Bucket is one token-bucket keyed dimension. It is pure over its inputs (the
// clock is passed in) so it is fully unit-testable with a fake clock, with no
// timers and no goroutines. ratePerMinute is the steady refill rate; burst is the
// capacity (the most that can be spent in an instant).
type bucket struct {
	ratePerMinute float64
	burst         float64
	tokens        float64
	last          time.Time
}

// newBucket returns a bucket full to its burst capacity.
func newBucket(ratePerMinute, burst float64) *bucket {
	return &bucket{ratePerMinute: ratePerMinute, burst: burst, tokens: burst}
}

// allow refills the bucket for the elapsed time since the last call, then spends
// n tokens if available. It reports whether the request is allowed. A
// non-positive rate means "unlimited" (always allow); this lets a tier leave a
// dimension uncapped by setting its rate to zero.
func (b *bucket) allow(now time.Time, n float64) bool {
	if b.ratePerMinute <= 0 {
		return true
	}
	if b.last.IsZero() {
		b.last = now
	}
	elapsed := now.Sub(b.last).Seconds()
	if elapsed > 0 {
		b.tokens += elapsed * (b.ratePerMinute / 60.0)
		if b.tokens > b.burst {
			b.tokens = b.burst
		}
		b.last = now
	}
	if b.tokens >= n {
		b.tokens -= n
		return true
	}
	return false
}

// BucketKind separates the lifecycle bucket (create/terminate/fork: the
// expensive, abuse-prone operations) from the in-sandbox bucket (exec, file,
// status: the in-VM traffic). They are independently rate-limited so a burst of
// cheap in-sandbox calls never starves the lifecycle budget and vice versa.
type BucketKind string

const (
	// BucketLifecycle is the create/terminate/fork request-rate bucket.
	BucketLifecycle BucketKind = "lifecycle"
	// BucketInSandbox is the exec/file/status in-sandbox request-rate bucket.
	BucketInSandbox BucketKind = "in_sandbox"
)

// RateLimiter holds a token bucket per (key, kind) pair. The key is an org id or
// an IP address: the same limiter enforces BOTH the per-org and the per-IP rate
// (the caller passes "org:<id>" and "ip:<addr>" keys). It is safe for concurrent
// use. The clock is injected so the limiter is deterministic under test.
//
// The rate and burst are passed per call, not stored, so an org's tier change
// takes effect on the next request without rebuilding the limiter; the bucket
// remembers only its token level and last-refill instant.
type RateLimiter struct {
	mu      sync.Mutex
	buckets map[bucketKey]*bucket
	now     func() time.Time
}

type bucketKey struct {
	key  string
	kind BucketKind
}

// NewRateLimiter builds a limiter. A nil clock defaults to time.Now.
func NewRateLimiter(now func() time.Time) *RateLimiter {
	if now == nil {
		now = time.Now
	}
	return &RateLimiter{buckets: map[bucketKey]*bucket{}, now: now}
}

// Allow charges one token against the (key, kind) bucket at the given
// ratePerMinute and burst, refilling for elapsed time first. It returns true if
// the request may proceed. A non-positive rate is unlimited. The burst defaults
// to the rate when non-positive, so a caller may pass only a rate.
func (rl *RateLimiter) Allow(key string, kind BucketKind, ratePerMinute, burst float64) bool {
	if burst <= 0 {
		burst = ratePerMinute
	}
	rl.mu.Lock()
	defer rl.mu.Unlock()
	bk := bucketKey{key: key, kind: kind}
	b := rl.buckets[bk]
	if b == nil {
		b = newBucket(ratePerMinute, burst)
		rl.buckets[bk] = b
	} else {
		// Keep the bucket's parameters current so a tier change is honored.
		b.ratePerMinute = ratePerMinute
		b.burst = burst
	}
	return b.allow(rl.now(), 1)
}
