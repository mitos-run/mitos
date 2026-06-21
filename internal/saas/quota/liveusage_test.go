package quota

import (
	"context"
	"testing"
	"time"

	"mitos.run/mitos/internal/usage"
)

// TestUsageBackedLiveUsageCountsDistinctSandboxes asserts the #211-usage-store-
// backed live-usage source reports the org's distinct running sandboxes in the
// trailing window as the concurrency proxy, and is org-scoped (never counts
// another org's sandboxes).
func TestUsageBackedLiveUsageCountsDistinctSandboxes(t *testing.T) {
	now := time.Unix(1_000_000, 0).UTC()
	store := usage.NewMemUsageStore()
	ctx := context.Background()
	win := now.Truncate(time.Minute)
	// org-1 has two distinct sandboxes (one with two window records); org-2 has one.
	for _, rec := range []usage.UsageRecord{
		{OrgID: "org-1", SandboxID: "sb-a", Window: win},
		{OrgID: "org-1", SandboxID: "sb-a", Window: win.Add(-time.Minute)},
		{OrgID: "org-1", SandboxID: "sb-b", Window: win},
		{OrgID: "org-2", SandboxID: "sb-z", Window: win},
	} {
		if err := store.UpsertRecord(ctx, rec); err != nil {
			t.Fatalf("upsert: %v", err)
		}
	}
	src := UsageBackedLiveUsage{Store: store, Window: 10 * time.Minute, Now: func() time.Time { return now }}

	lu1, err := src.Live(ctx, "org-1")
	if err != nil {
		t.Fatalf("Live org-1: %v", err)
	}
	if lu1.ConcurrentSandboxes != 2 {
		t.Fatalf("org-1 concurrency = %d, want 2 distinct sandboxes", lu1.ConcurrentSandboxes)
	}
	lu2, err := src.Live(ctx, "org-2")
	if err != nil {
		t.Fatalf("Live org-2: %v", err)
	}
	if lu2.ConcurrentSandboxes != 1 {
		t.Fatalf("org-2 concurrency = %d, want 1 (org-scoped, not org-1's)", lu2.ConcurrentSandboxes)
	}
}

// TestLiveCounterSourceWraps asserts the live-count seam (the preferred,
// authoritative concurrency source) plugs in as a LiveUsageSource.
func TestLiveCounterSourceWraps(t *testing.T) {
	src := NewLiveCounterSource(fakeCounter{lu: LiveUsage{ConcurrentSandboxes: 3, VCPUs: 6}})
	lu, err := src.Live(context.Background(), "org-1")
	if err != nil {
		t.Fatalf("Live: %v", err)
	}
	if lu.ConcurrentSandboxes != 3 || lu.VCPUs != 6 {
		t.Fatalf("live = %+v, want {3, 6}", lu)
	}
}

type fakeCounter struct{ lu LiveUsage }

func (f fakeCounter) Count(_ context.Context, _ string) (LiveUsage, error) { return f.lu, nil }
