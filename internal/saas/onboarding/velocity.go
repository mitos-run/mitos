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
	// lastSweep is the last time every key was pruned. A key that is hit once and
	// never again is only pruned lazily on its own access, so without a periodic
	// full sweep a flood of unique (never-returning) source IPs would grow the map
	// without bound. The sweep runs at most once per window and bounds the map to
	// the set of keys active within the last window.
	lastSweep time.Time
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

	cutoff := now.Add(-v.window)

	// Periodic full sweep: reclaim keys whose timestamps are all expired, so a
	// flood of unique source IPs that each hit once cannot grow the map without
	// bound. Runs at most once per window (amortized O(1) per Allow).
	if now.Sub(v.lastSweep) >= v.window {
		for k, ts := range v.hits {
			live := ts[:0]
			for _, t := range ts {
				if t.After(cutoff) {
					live = append(live, t)
				}
			}
			if len(live) == 0 {
				delete(v.hits, k)
			} else {
				v.hits[k] = live
			}
		}
		v.lastSweep = now
	}

	// Prune timestamps that have fallen outside the sliding window for this key.
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
