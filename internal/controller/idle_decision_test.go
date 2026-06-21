package controller

import (
	"testing"
	"time"
)

// TestIdleDecisionWorkAware is the work-aware idle contract (issue #218): idle
// is measured against ACTUAL activity, which includes a running background job
// (a live stream), not just inbound API interaction. A sandbox whose last
// interaction is far past its idle window but that still has a live background
// process is NOT idle, so an unattended job is never reaped mid-run (the
// Daytona weakness we beat).
func TestIdleDecisionWorkAware(t *testing.T) {
	idle := 5 * time.Minute
	started := time.Unix(1_700_000_000, 0)
	now := started.Add(1 * time.Hour) // long past any interaction-only idle window

	cases := []struct {
		name        string
		sig         activitySignal
		wantExpired bool
	}{
		{
			name:        "interaction-only idle, no work: reaped",
			sig:         activitySignal{LastActivity: started, ActiveStreams: 0, Paused: false},
			wantExpired: true,
		},
		{
			name:        "background job alive: NOT reaped despite no interaction",
			sig:         activitySignal{LastActivity: started, ActiveStreams: 1, Paused: false},
			wantExpired: false,
		},
		{
			name:        "paused: clock stopped, NOT reaped",
			sig:         activitySignal{LastActivity: started, ActiveStreams: 0, Paused: true},
			wantExpired: false,
		},
		{
			name:        "recent interaction: NOT reaped",
			sig:         activitySignal{LastActivity: now.Add(-1 * time.Minute), ActiveStreams: 0, Paused: false},
			wantExpired: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := idleExpired(tc.sig, started, idle, now)
			if got != tc.wantExpired {
				t.Fatalf("idleExpired = %v, want %v", got, tc.wantExpired)
			}
		})
	}
}

// TestLiveDeadlineHonored asserts the set_timeout live TTL is honored: when a
// live deadline is set (issue #218), the sandbox is reaped once now passes it,
// independent of the idle clock. A deadline in the future keeps the sandbox
// alive even past the idle window (the caller explicitly extended it).
func TestLiveDeadlineHonored(t *testing.T) {
	idle := 5 * time.Minute
	started := time.Unix(1_700_000_000, 0)

	// Live deadline in the future: not expired even though idle would have fired.
	future := started.Add(2 * time.Hour)
	now := started.Add(1 * time.Hour)
	sig := activitySignal{LastActivity: started, Deadline: future, ActiveStreams: 0}
	if deadlineExpired(sig, now) {
		t.Fatal("a future live deadline must not be expired")
	}

	// Live deadline in the past: expired.
	past := started.Add(10 * time.Minute)
	if !deadlineExpired(activitySignal{Deadline: past}, now) {
		t.Fatal("a past live deadline must be expired")
	}

	// No live deadline set: deadlineExpired is always false (idle clock governs).
	if deadlineExpired(activitySignal{}, now) {
		t.Fatal("an unset live deadline must never be expired")
	}
	_ = idle
}
