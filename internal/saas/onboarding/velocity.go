package onboarding

import (
	"sync"
	"time"
)

// Velocity is an in-memory per-key sliding-window rate limiter. Construct with
// NewVelocity. A limit of 0 or less disables the cap so Allow always returns
// true. It is safe for concurrent use.
type Velocity struct {
	limit  int
	window time.Duration
	mu     sync.Mutex
	hits   map[string][]time.Time
}

// NewVelocity returns a Velocity that allows at most limit attempts per key
// inside the sliding window. A limit of 0 or less yields a disabled limiter
// where Allow always returns true (self-host safe).
func NewVelocity(limit int, window time.Duration) *Velocity {
	return &Velocity{
		limit:  limit,
		window: window,
		hits:   make(map[string][]time.Time),
	}
}

// Allow records an attempt for key at now and reports whether it is within the
// cap. It returns false when the key has already reached the limit inside the
// sliding window ending at now, without recording the excess attempt. A nil
// *Velocity always returns true (disabled).
//
// The caller passes now explicitly so tests can drive the window with a fixed
// clock. Allow never calls time.Now() internally.
func (v *Velocity) Allow(key string, now time.Time) bool {
	if v == nil || v.limit <= 0 {
		return true
	}
	v.mu.Lock()
	defer v.mu.Unlock()

	// Prune timestamps that have fallen outside the sliding window.
	cutoff := now.Add(-v.window)
	existing := v.hits[key]
	var fresh []time.Time
	for _, t := range existing {
		if t.After(cutoff) {
			fresh = append(fresh, t)
		}
	}

	if len(fresh) >= v.limit {
		// Over the cap: store the pruned slice but do not record this attempt.
		if len(fresh) > 0 {
			v.hits[key] = fresh
		} else {
			delete(v.hits, key)
		}
		return false
	}

	// Within the cap: record this attempt and allow it.
	v.hits[key] = append(fresh, now)
	return true
}
