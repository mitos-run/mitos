package controller

// Unit coverage for the activation retry backoff (issue #894): a claim that
// cannot activate must back off from the flat capacityPendingRequeue cadence as
// the failure persists, capped, so it stops hammering forkd and the apiserver.

import (
	"testing"
	"time"
)

func TestActivateRetryBackoff(t *testing.T) {
	cases := []struct {
		name   string
		waited time.Duration
		want   time.Duration
	}{
		{"fresh failure returns the base cadence", 0, capacityPendingRequeue},
		{"just under the first double stays at base", 9 * time.Second, capacityPendingRequeue},
		{"at the first double grows to 10s", 10 * time.Second, 10 * time.Second},
		{"between doubles holds the lower step", 15 * time.Second, 10 * time.Second},
		{"at the second double grows to 20s", 20 * time.Second, 20 * time.Second},
		{"past the cap threshold clamps to the max", 40 * time.Second, activateFailingMaxBackoff},
		{"far past clamps to the max", 5 * time.Minute, activateFailingMaxBackoff},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := activateRetryBackoff(c.waited); got != c.want {
				t.Errorf("activateRetryBackoff(%s) = %s, want %s", c.waited, got, c.want)
			}
		})
	}
}

// The backoff must never decrease as the failure persists and never exceed the
// cap: a regression either way reintroduces the full-cadence hammering (#894).
func TestActivateRetryBackoffMonotonicAndCapped(t *testing.T) {
	var prev time.Duration
	for w := time.Duration(0); w <= 10*time.Minute; w += time.Second {
		got := activateRetryBackoff(w)
		if got < prev {
			t.Fatalf("backoff decreased at waited=%s: %s < %s", w, got, prev)
		}
		if got > activateFailingMaxBackoff {
			t.Fatalf("backoff exceeded the cap at waited=%s: %s > %s", w, got, activateFailingMaxBackoff)
		}
		if got < capacityPendingRequeue {
			t.Fatalf("backoff below the base cadence at waited=%s: %s < %s", w, got, capacityPendingRequeue)
		}
		prev = got
	}
}
