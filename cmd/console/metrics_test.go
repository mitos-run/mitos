package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"mitos.run/mitos/internal/saas"
	"mitos.run/mitos/internal/saas/billing"
	"mitos.run/mitos/internal/usage"
)

// TestDrawdownMetricsCleanCycle asserts a clean cycle stamps the last-success
// gauge to the cycle time, adds nothing to the error counter, and counts a
// credit-exhausted record when the settle result carries an unbacked
// remainder.
func TestDrawdownMetricsCleanCycle(t *testing.T) {
	now := time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC)
	w1 := now.Add(-10 * time.Minute)
	orgs := &fakeOrgLister{orgs: []saas.Organization{{ID: "org-a"}}}
	store := &fakeRecordLister{records: map[string][]usage.UsageRecord{
		"org-a": {
			{OrgID: "org-a", SandboxID: "sb-1", Window: w1},
			{OrgID: "org-a", SandboxID: "sb-2", Window: w1},
		},
	}}
	// sb-1 settles fully from credit; sb-2 exceeds the remaining credit.
	svc := &fakeDrawdowner{results: map[string]billing.DrawdownResult{
		"org-a|sb-1|" + w1.UTC().Format(time.RFC3339): {Cost: 5, FromCredit: 5, Remaining: 0},
		"org-a|sb-2|" + w1.UTC().Format(time.RFC3339): {Cost: 7, FromCredit: 2, Remaining: 5},
	}}
	m := newDrawdownMetrics()
	m.mustRegister(prometheus.NewRegistry())

	runDrawdownOnce(context.Background(), testLogger(), orgs, store, svc, 2*time.Hour, now, m)

	if got := testutil.ToFloat64(m.lastSuccess); got != float64(now.Unix()) {
		t.Errorf("lastSuccess = %v, want %v", got, now.Unix())
	}
	if got := testutil.ToFloat64(m.cycleErrors); got != 0 {
		t.Errorf("cycleErrors = %v, want 0", got)
	}
	if got := testutil.ToFloat64(m.creditExhausted); got != 1 {
		t.Errorf("creditExhausted = %v, want 1 (only sb-2 exceeded credit)", got)
	}
}

// TestDrawdownMetricsFailingCycle asserts a cycle with failed operations adds
// them to the error counter and does NOT move the last-success gauge (the
// DrawdownStalled staleness keeps growing).
func TestDrawdownMetricsFailingCycle(t *testing.T) {
	now := time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC)
	orgs := &fakeOrgLister{err: errors.New("store down")}
	m := newDrawdownMetrics()
	m.mustRegister(prometheus.NewRegistry())
	m.markStarted(now.Add(-time.Hour))

	runDrawdownOnce(context.Background(), testLogger(), orgs, &fakeRecordLister{}, &fakeDrawdowner{}, 2*time.Hour, now, m)

	if got := testutil.ToFloat64(m.cycleErrors); got != 1 {
		t.Errorf("cycleErrors = %v, want 1", got)
	}
	if got := testutil.ToFloat64(m.lastSuccess); got != float64(now.Add(-time.Hour).Unix()) {
		t.Errorf("lastSuccess = %v, want the start stamp (a failing cycle must not refresh it)", got)
	}
}

// TestDrawdownMetricsReplayIsNotExhaustion asserts a replayed record (already
// settled) never counts as credit exhaustion, even if its recovered result
// carries a nonzero remainder shape.
func TestDrawdownMetricsReplayIsNotExhaustion(t *testing.T) {
	now := time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC)
	w1 := now.Add(-10 * time.Minute)
	orgs := &fakeOrgLister{orgs: []saas.Organization{{ID: "org-a"}}}
	store := &fakeRecordLister{records: map[string][]usage.UsageRecord{
		"org-a": {{OrgID: "org-a", SandboxID: "sb-1", Window: w1}},
	}}
	svc := &fakeDrawdowner{
		settled: map[string]map[string]bool{
			"org-a": {billing.DrawdownKey("org-a", "sb-1", w1): true},
		},
	}
	m := newDrawdownMetrics()
	m.mustRegister(prometheus.NewRegistry())

	stats := runDrawdownOnce(context.Background(), testLogger(), orgs, store, svc, 2*time.Hour, now, m)

	if stats.replayed != 1 {
		t.Fatalf("replayed = %d, want 1", stats.replayed)
	}
	if got := testutil.ToFloat64(m.creditExhausted); got != 0 {
		t.Errorf("creditExhausted = %v, want 0 for a replay", got)
	}
}
