package main

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"mitos.run/mitos/internal/saas"
	"mitos.run/mitos/internal/saas/billing"
	"mitos.run/mitos/internal/usage"
)

type fakeOrgLister struct {
	orgs []saas.Organization
	err  error
}

func (f *fakeOrgLister) ListOrgs(_ context.Context) ([]saas.Organization, error) {
	return f.orgs, f.err
}

type fakeRecordLister struct {
	records map[string][]usage.UsageRecord
	froms   map[string]time.Time
	tos     map[string]time.Time
	err     error
}

func (f *fakeRecordLister) ListRecords(_ context.Context, orgID string, from, to time.Time) ([]usage.UsageRecord, error) {
	if f.froms == nil {
		f.froms = map[string]time.Time{}
		f.tos = map[string]time.Time{}
	}
	f.froms[orgID] = from
	f.tos[orgID] = to
	if f.err != nil {
		return nil, f.err
	}
	return f.records[orgID], nil
}

type fakeDrawdowner struct {
	keys []string
	err  error
	// results maps a record key (org|sandbox|window) to the result Drawdown
	// returns for it, so a test can simulate per-record settled cents and
	// carried remainders. A record with no mapped result returns the zero result.
	results map[string]billing.DrawdownResult
	// settled maps orgID to the already-settled drawdown keys (DrawdownKey form)
	// SettledWindowKeys returns; settledErr fails that read.
	settled    map[string]map[string]bool
	settledErr error
	// settledSince records the since bound of each SettledWindowKeys call;
	// pruneOlderThan records each PruneProcessedWindows horizon.
	settledSince   map[string]time.Time
	pruneOlderThan []time.Time
	pruneErr       error
	pruned         int64
	// capChecks records the orgs whose spend cap was evaluated; capSuspends maps
	// an org to the suspended result; capErr fails every cap evaluation.
	capChecks   []string
	capSuspends map[string]bool
	capErr      error
}

func (f *fakeDrawdowner) Drawdown(_ context.Context, rec usage.UsageRecord) (billing.DrawdownResult, error) {
	key := rec.OrgID + "|" + rec.SandboxID + "|" + rec.Window.UTC().Format(time.RFC3339)
	f.keys = append(f.keys, key)
	return f.results[key], f.err
}

func (f *fakeDrawdowner) SettledWindowKeys(_ context.Context, orgID string, since time.Time) (map[string]bool, error) {
	if f.settledSince == nil {
		f.settledSince = map[string]time.Time{}
	}
	f.settledSince[orgID] = since
	if f.settledErr != nil {
		return nil, f.settledErr
	}
	return f.settled[orgID], nil
}

func (f *fakeDrawdowner) PruneProcessedWindows(_ context.Context, olderThan time.Time) (int64, error) {
	f.pruneOlderThan = append(f.pruneOlderThan, olderThan)
	if f.pruneErr != nil {
		return 0, f.pruneErr
	}
	return f.pruned, nil
}

func (f *fakeDrawdowner) EnforceSpendCapFromLedger(_ context.Context, orgID string) (bool, error) {
	f.capChecks = append(f.capChecks, orgID)
	if f.capErr != nil {
		return false, f.capErr
	}
	return f.capSuspends[orgID], nil
}

func testLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

// TestRunDrawdownOnceDrawsDownEveryRecord: one tick lists every org, fetches
// each org's recent finalized usage records, and calls Drawdown once per
// record. Idempotency across ticks is the billing service's job (the record key
// is the ledger idempotency key); the driver just replays the window.
func TestRunDrawdownOnceDrawsDownEveryRecord(t *testing.T) {
	now := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
	w1 := now.Add(-10 * time.Minute)
	w2 := now.Add(-9 * time.Minute)
	orgs := &fakeOrgLister{orgs: []saas.Organization{{ID: "org-a"}, {ID: "org-b"}}}
	store := &fakeRecordLister{records: map[string][]usage.UsageRecord{
		"org-a": {
			{OrgID: "org-a", SandboxID: "sb-1", Window: w1},
			{OrgID: "org-a", SandboxID: "sb-1", Window: w2},
		},
		"org-b": {
			{OrgID: "org-b", SandboxID: "sb-2", Window: w1},
		},
	}}
	svc := &fakeDrawdowner{}

	stats := runDrawdownOnce(context.Background(), testLogger(), orgs, store, svc, 2*time.Hour, now, nil)

	if len(svc.keys) != 3 {
		t.Fatalf("Drawdown called %d times, want 3 (once per record): %v", len(svc.keys), svc.keys)
	}
	if stats.records != 3 || stats.drawn != 3 || stats.failed != 0 {
		t.Errorf("stats = %+v, want records=3 drawn=3 failed=0", stats)
	}
	// The listing window is the lookback up to the last FINALIZED usage window:
	// the still-open current window must be excluded, or the idempotent drawdown
	// would lock in a partial cost for it.
	wantFrom := now.Add(-2 * time.Hour)
	wantTo := now.Add(-usage.DefaultConfig().Window)
	for _, org := range []string{"org-a", "org-b"} {
		if !store.froms[org].Equal(wantFrom) {
			t.Errorf("org %s listed from %v, want %v", org, store.froms[org], wantFrom)
		}
		if !store.tos[org].Equal(wantTo) {
			t.Errorf("org %s listed to %v, want %v (exclude the open window)", org, store.tos[org], wantTo)
		}
	}
}

// TestRunDrawdownOnceCountsFailuresAndContinues: a failing org listing or a
// failing drawdown is counted and never aborts the other orgs' settlement.
func TestRunDrawdownOnceCountsFailuresAndContinues(t *testing.T) {
	now := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
	orgs := &fakeOrgLister{orgs: []saas.Organization{{ID: "org-a"}, {ID: "org-b"}}}
	store := &fakeRecordLister{records: map[string][]usage.UsageRecord{
		"org-a": {{OrgID: "org-a", SandboxID: "sb-1", Window: now.Add(-5 * time.Minute)}},
		"org-b": {{OrgID: "org-b", SandboxID: "sb-2", Window: now.Add(-5 * time.Minute)}},
	}}
	svc := &fakeDrawdowner{err: errors.New("ledger down")}

	stats := runDrawdownOnce(context.Background(), testLogger(), orgs, store, svc, time.Hour, now, nil)

	if len(svc.keys) != 2 {
		t.Fatalf("Drawdown called %d times, want 2 (a failed drawdown must not abort the cycle)", len(svc.keys))
	}
	if stats.drawn != 0 || stats.failed != 2 {
		t.Errorf("stats = %+v, want drawn=0 failed=2", stats)
	}
}

// TestRunDrawdownOnceEnforcesSpendCapPerActiveOrg asserts the cycle evaluates
// the spend cap for every org that had records in the lookback (the issue #615
// production path: settling usage is the moment spend can newly breach a cap)
// and skips idle orgs, that a suspending evaluation is counted, and that a cap
// evaluation error is counted as a failure without aborting the cycle.
func TestRunDrawdownOnceEnforcesSpendCapPerActiveOrg(t *testing.T) {
	now := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
	orgs := &fakeOrgLister{orgs: []saas.Organization{{ID: "org-a"}, {ID: "org-idle"}}}
	store := &fakeRecordLister{records: map[string][]usage.UsageRecord{
		"org-a": {{OrgID: "org-a", SandboxID: "sb-1", Window: now.Add(-10 * time.Minute)}},
	}}
	svc := &fakeDrawdowner{capSuspends: map[string]bool{"org-a": true}}

	stats := runDrawdownOnce(context.Background(), testLogger(), orgs, store, svc, 2*time.Hour, now, nil)

	if len(svc.capChecks) != 1 || svc.capChecks[0] != "org-a" {
		t.Fatalf("cap checks = %v, want exactly [org-a] (idle orgs skip the scan)", svc.capChecks)
	}
	if stats.suspended != 1 {
		t.Errorf("stats.suspended = %d, want 1", stats.suspended)
	}
	if stats.failed != 0 {
		t.Errorf("stats.failed = %d, want 0", stats.failed)
	}

	// A cap evaluation error is a counted failure, never a cycle abort.
	svcErr := &fakeDrawdowner{capErr: errors.New("caps store down")}
	stats = runDrawdownOnce(context.Background(), testLogger(), orgs, store, svcErr, 2*time.Hour, now, nil)
	if len(svcErr.capChecks) != 1 {
		t.Fatalf("cap checks with error = %v, want the active org still checked", svcErr.capChecks)
	}
	if stats.failed == 0 {
		t.Error("a cap evaluation error must count as a failure")
	}
	if stats.suspended != 0 {
		t.Errorf("stats.suspended = %d, want 0 on error", stats.suspended)
	}
}

// TestDrawdownIntervalResolution pins the env contract: default 5m when the
// usage store is the live HTTP store, OFF when the store is the in-memory dev
// fallback (nothing real to settle), explicit 0/off disables, and an explicit
// duration wins.
func TestDrawdownIntervalResolution(t *testing.T) {
	cases := []struct {
		raw     string
		live    bool
		want    time.Duration
		wantErr bool
	}{
		{raw: "", live: true, want: 5 * time.Minute},
		{raw: "", live: false, want: 0},
		{raw: "0", live: true, want: 0},
		{raw: "off", live: true, want: 0},
		{raw: "2m", live: false, want: 2 * time.Minute},
		{raw: "bogus", live: true, wantErr: true},
	}
	for _, c := range cases {
		got, err := drawdownInterval(c.raw, c.live)
		if c.wantErr {
			if err == nil {
				t.Errorf("drawdownInterval(%q, live=%t): want error, got %v", c.raw, c.live, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("drawdownInterval(%q, live=%t): %v", c.raw, c.live, err)
			continue
		}
		if got != c.want {
			t.Errorf("drawdownInterval(%q, live=%t) = %v, want %v", c.raw, c.live, got, c.want)
		}
	}
}

// TestRunDrawdownOnceReportsSettledCents pins the issue #662/#665 visibility
// contract: the cycle stats carry the AGGREGATE settled cents (sum of every
// record's FromCredit) and the aggregate carried milli-cent remainder (each
// org's remainder after its last settled record), so a system that settles
// zero forever is visible in the drawdown log line instead of looking healthy.
func TestRunDrawdownOnceReportsSettledCents(t *testing.T) {
	now := time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)
	w1 := now.Add(-10 * time.Minute)
	w2 := now.Add(-9 * time.Minute)
	orgs := &fakeOrgLister{orgs: []saas.Organization{{ID: "org-a"}, {ID: "org-b"}}}
	store := &fakeRecordLister{records: map[string][]usage.UsageRecord{
		"org-a": {
			{OrgID: "org-a", SandboxID: "sb-1", Window: w1},
			{OrgID: "org-a", SandboxID: "sb-1", Window: w2},
		},
		"org-b": {
			{OrgID: "org-b", SandboxID: "sb-2", Window: w1},
		},
	}}
	key := func(org, sb string, w time.Time) string { return org + "|" + sb + "|" + w.UTC().Format(time.RFC3339) }
	svc := &fakeDrawdowner{results: map[string]billing.DrawdownResult{
		key("org-a", "sb-1", w1): {Cost: 1, FromCredit: 1, CarriedMilliCents: 100},
		key("org-a", "sb-1", w2): {Cost: 0, FromCredit: 0, CarriedMilliCents: 177},
		key("org-b", "sb-2", w1): {Cost: 2, FromCredit: 2, CarriedMilliCents: -300},
	}}

	stats := runDrawdownOnce(context.Background(), testLogger(), orgs, store, svc, 2*time.Hour, now, nil)

	if stats.settledCents != 3 {
		t.Errorf("settledCents = %d, want 3 (1 from org-a + 2 from org-b)", stats.settledCents)
	}
	// Per org, the LAST settled record's carried remainder is the org's current
	// remainder; the stat is the sum across orgs: 177 + (-300).
	if stats.carriedMilli != -123 {
		t.Errorf("carriedMilli = %d, want -123 (org-a 177 + org-b -300)", stats.carriedMilli)
	}
}

// TestRunDrawdownOnceSkipsSettledWindowsBeforePricing is the issue #672 core:
// a window in the org's settled skip set is never handed to Drawdown at all
// (no wasted pricing), counts as replayed, and contributes nothing to
// settledCents. The skip set is read once per org with the lookback start as
// the since bound.
func TestRunDrawdownOnceSkipsSettledWindowsBeforePricing(t *testing.T) {
	now := time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)
	w1 := now.Add(-10 * time.Minute)
	w2 := now.Add(-9 * time.Minute)
	orgs := &fakeOrgLister{orgs: []saas.Organization{{ID: "org-a"}}}
	store := &fakeRecordLister{records: map[string][]usage.UsageRecord{
		"org-a": {
			{OrgID: "org-a", SandboxID: "sb-1", Window: w1},
			{OrgID: "org-a", SandboxID: "sb-1", Window: w2},
		},
	}}
	svc := &fakeDrawdowner{
		settled: map[string]map[string]bool{
			"org-a": {billing.DrawdownKey("org-a", "sb-1", w1): true},
		},
		results: map[string]billing.DrawdownResult{
			"org-a|sb-1|" + w2.UTC().Format(time.RFC3339): {Cost: 2, FromCredit: 2, CarriedMilliCents: 40},
		},
	}

	stats := runDrawdownOnce(context.Background(), testLogger(), orgs, store, svc, 2*time.Hour, now, nil)

	if len(svc.keys) != 1 {
		t.Fatalf("Drawdown called %d times, want 1 (the settled window must be skipped before pricing): %v", len(svc.keys), svc.keys)
	}
	if stats.records != 2 || stats.drawn != 1 || stats.replayed != 1 || stats.failed != 0 {
		t.Errorf("stats = %+v, want records=2 drawn=1 replayed=1 failed=0", stats)
	}
	if stats.settledCents != 2 {
		t.Errorf("settledCents = %d, want 2 (only the new window)", stats.settledCents)
	}
	wantSince := now.Add(-2 * time.Hour)
	if !svc.settledSince["org-a"].Equal(wantSince) {
		t.Errorf("SettledWindowKeys since = %v, want the lookback start %v", svc.settledSince["org-a"], wantSince)
	}
}

// TestRunDrawdownOnceCountsSettleTimeReplays: if the skip set misses a settled
// window (settled between the read and the settle), the ledger dedup still
// fires and Drawdown reports Replayed; the driver counts it as replayed and
// keeps its prior credit OUT of settledCents (the pre-#672 over-report bug).
func TestRunDrawdownOnceCountsSettleTimeReplays(t *testing.T) {
	now := time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)
	w1 := now.Add(-10 * time.Minute)
	orgs := &fakeOrgLister{orgs: []saas.Organization{{ID: "org-a"}}}
	store := &fakeRecordLister{records: map[string][]usage.UsageRecord{
		"org-a": {{OrgID: "org-a", SandboxID: "sb-1", Window: w1}},
	}}
	svc := &fakeDrawdowner{results: map[string]billing.DrawdownResult{
		"org-a|sb-1|" + w1.UTC().Format(time.RFC3339): {Cost: 5, FromCredit: 5, CarriedMilliCents: 100, Replayed: true},
	}}

	stats := runDrawdownOnce(context.Background(), testLogger(), orgs, store, svc, 2*time.Hour, now, nil)

	if stats.replayed != 1 || stats.drawn != 0 {
		t.Errorf("stats = %+v, want replayed=1 drawn=0", stats)
	}
	if stats.settledCents != 0 {
		t.Errorf("settledCents = %d, want 0 (a replay's prior credit must not be re-counted)", stats.settledCents)
	}
	if stats.carriedMilli != 0 {
		t.Errorf("carriedMilli = %d, want 0 (no NEW settle happened for the org)", stats.carriedMilli)
	}
}

// TestRunDrawdownOnceSkipSetErrorDefersOrg: an org whose settled skip set
// cannot be read is deferred whole (counted as a failure, no records priced);
// settling blind would re-count every replay into settledCents.
func TestRunDrawdownOnceSkipSetErrorDefersOrg(t *testing.T) {
	now := time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)
	orgs := &fakeOrgLister{orgs: []saas.Organization{{ID: "org-a"}}}
	store := &fakeRecordLister{records: map[string][]usage.UsageRecord{
		"org-a": {{OrgID: "org-a", SandboxID: "sb-1", Window: now.Add(-5 * time.Minute)}},
	}}
	svc := &fakeDrawdowner{settledErr: errors.New("ledger down")}

	stats := runDrawdownOnce(context.Background(), testLogger(), orgs, store, svc, time.Hour, now, nil)

	if len(svc.keys) != 0 {
		t.Fatalf("Drawdown called %d times, want 0 (org must be deferred when the skip set is unreadable)", len(svc.keys))
	}
	if stats.failed != 1 || stats.drawn != 0 {
		t.Errorf("stats = %+v, want failed=1 drawn=0", stats)
	}
}

// TestRunDrawdownOncePrunesAgedMarkers: each cycle prunes processed-window
// markers older than the lookback start (they can never be listed again) and
// reports the count; a prune failure is counted, never fatal.
func TestRunDrawdownOncePrunesAgedMarkers(t *testing.T) {
	now := time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)
	orgs := &fakeOrgLister{}
	store := &fakeRecordLister{}
	svc := &fakeDrawdowner{pruned: 7}

	stats := runDrawdownOnce(context.Background(), testLogger(), orgs, store, svc, 2*time.Hour, now, nil)

	if len(svc.pruneOlderThan) != 1 || !svc.pruneOlderThan[0].Equal(now.Add(-2*time.Hour)) {
		t.Fatalf("prune calls = %v, want exactly one at the lookback start %v", svc.pruneOlderThan, now.Add(-2*time.Hour))
	}
	if stats.pruned != 7 {
		t.Errorf("pruned = %d, want 7", stats.pruned)
	}

	failing := &fakeDrawdowner{pruneErr: errors.New("db down")}
	stats = runDrawdownOnce(context.Background(), testLogger(), orgs, store, failing, 2*time.Hour, now, nil)
	if stats.failed != 1 {
		t.Errorf("stats after prune failure = %+v, want failed=1", stats)
	}
}

// realBillingHarness wires the REAL billing.Service over the in-memory ledger
// so the driver tests below prove the end-to-end tick behavior: money moves
// exactly once per window regardless of how often the lookback replays it.
func realBillingHarness(t *testing.T, orgID string) (*billing.Service, *billing.MemCreditLedger) {
	t.Helper()
	l := billing.NewMemCreditLedger()
	if err := billing.GrantSignupCredit(context.Background(), l, orgID, billing.USD(100), time.Date(2026, 7, 3, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("grant: %v", err)
	}
	return billing.NewService(billing.Config{Stripe: billing.NewFakeStripe(), Ledger: l, Rates: billing.DefaultRates()}), l
}

// centRecord is a usage record costing exactly 128 cents at DefaultRates
// (100k vCPU-seconds), so settles are whole-cent and carry-free.
func centRecord(org, sb string, w time.Time) usage.UsageRecord {
	return usage.UsageRecord{OrgID: org, SandboxID: sb, Window: w, VCPUSeconds: 100_000}
}

// TestRunDrawdownReplayAcrossTicksBillsOnce is the issue #672 acceptance
// against the real billing service: the lookback re-lists settled windows on
// every tick, but each window debits credit exactly once, later ticks report
// the replays as replayedRecords, and settledCents covers only the appends
// that actually landed that tick.
func TestRunDrawdownReplayAcrossTicksBillsOnce(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)
	w1 := now.Add(-10 * time.Minute)
	w2 := now.Add(-9 * time.Minute)
	w3 := now.Add(-8 * time.Minute)
	svc, ledger := realBillingHarness(t, "org-a")
	orgs := &fakeOrgLister{orgs: []saas.Organization{{ID: "org-a"}}}
	store := &fakeRecordLister{records: map[string][]usage.UsageRecord{
		"org-a": {centRecord("org-a", "sb-1", w1), centRecord("org-a", "sb-1", w2)},
	}}

	tick1 := runDrawdownOnce(ctx, testLogger(), orgs, store, svc, 2*time.Hour, now, nil)
	if tick1.drawn != 2 || tick1.replayed != 0 || tick1.settledCents != 256 {
		t.Fatalf("tick 1 = %+v, want drawn=2 replayed=0 settledCents=256", tick1)
	}

	// Tick 2 re-lists both settled windows (the lookback overlap) plus one new.
	store.records["org-a"] = append(store.records["org-a"], centRecord("org-a", "sb-1", w3))
	tick2 := runDrawdownOnce(ctx, testLogger(), orgs, store, svc, 2*time.Hour, now.Add(5*time.Minute), nil)
	if tick2.drawn != 1 || tick2.replayed != 2 {
		t.Fatalf("tick 2 = %+v, want drawn=1 replayed=2", tick2)
	}
	if tick2.settledCents != 128 {
		t.Errorf("tick 2 settledCents = %d, want 128 (only the NEW window; pre-#672 this read 384)", tick2.settledCents)
	}

	// Tick 3 is a pure replay: nothing new, nothing settled, nothing debited.
	tick3 := runDrawdownOnce(ctx, testLogger(), orgs, store, svc, 2*time.Hour, now.Add(10*time.Minute), nil)
	if tick3.drawn != 0 || tick3.replayed != 3 || tick3.settledCents != 0 {
		t.Fatalf("tick 3 = %+v, want drawn=0 replayed=3 settledCents=0", tick3)
	}

	bal, _ := ledger.Balance(ctx, "org-a")
	if bal != billing.USD(100)-384 {
		t.Errorf("balance = %d, want %d (three windows debited exactly once each)", int64(bal), int64(billing.USD(100)-384))
	}
	entries, _ := ledger.Entries(ctx, "org-a")
	var drawdownRows int
	for _, e := range entries {
		if e.Kind == billing.KindUsageDrawdown {
			drawdownRows++
		}
	}
	if drawdownRows != 3 {
		t.Errorf("usage_drawdown rows = %d, want 3 (one per window, no replay rows)", drawdownRows)
	}
}

// TestRunDrawdownLateArrivingWindowSettlesOnce proves the reason the skip set
// is keyed per window and NOT a per-org watermark: windows can settle out of
// order (a transient per-record failure or a late-listed record leaves an
// OLDER window unsettled while a newer one settles), and such a late window
// must still settle exactly once when it appears inside the lookback.
func TestRunDrawdownLateArrivingWindowSettlesOnce(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)
	wLate := now.Add(-30 * time.Minute) // older, arrives AFTER wNew settles.
	wNew := now.Add(-9 * time.Minute)
	svc, ledger := realBillingHarness(t, "org-a")
	orgs := &fakeOrgLister{orgs: []saas.Organization{{ID: "org-a"}}}
	store := &fakeRecordLister{records: map[string][]usage.UsageRecord{
		"org-a": {centRecord("org-a", "sb-1", wNew)},
	}}

	tick1 := runDrawdownOnce(ctx, testLogger(), orgs, store, svc, 2*time.Hour, now, nil)
	if tick1.drawn != 1 || tick1.settledCents != 128 {
		t.Fatalf("tick 1 = %+v, want drawn=1 settledCents=128", tick1)
	}

	// The older window shows up late, still inside the lookback. A watermark at
	// wNew would skip it forever; the per-window skip set settles it.
	store.records["org-a"] = []usage.UsageRecord{
		centRecord("org-a", "sb-1", wLate),
		centRecord("org-a", "sb-1", wNew),
	}
	tick2 := runDrawdownOnce(ctx, testLogger(), orgs, store, svc, 2*time.Hour, now.Add(5*time.Minute), nil)
	if tick2.drawn != 1 || tick2.replayed != 1 || tick2.settledCents != 128 {
		t.Fatalf("tick 2 = %+v, want drawn=1 replayed=1 settledCents=128 (the late window settles once)", tick2)
	}

	// And only once: the next tick replays both.
	tick3 := runDrawdownOnce(ctx, testLogger(), orgs, store, svc, 2*time.Hour, now.Add(10*time.Minute), nil)
	if tick3.drawn != 0 || tick3.replayed != 2 || tick3.settledCents != 0 {
		t.Fatalf("tick 3 = %+v, want drawn=0 replayed=2 settledCents=0", tick3)
	}

	bal, _ := ledger.Balance(ctx, "org-a")
	if bal != billing.USD(100)-256 {
		t.Errorf("balance = %d, want %d (both windows debited exactly once)", int64(bal), int64(billing.USD(100)-256))
	}
}

// TestRunDrawdownSkipsWindowsSettledBeforeMarkerTable pins the deploy
// transition: a window settled BEFORE the processed-window mechanism exists
// only as a keyed credit_ledger row. The first post-deploy tick must treat it
// as settled (the skip set consults both mechanisms), never re-price or
// re-debit it. This dual consult is removable one lookback horizon after the
// deploy.
func TestRunDrawdownSkipsWindowsSettledBeforeMarkerTable(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)
	w1 := now.Add(-10 * time.Minute)
	svc, ledger := realBillingHarness(t, "org-a")
	// Simulate the pre-#672 settle: a keyed ledger row via plain Append (the
	// old AppendWithRemainder wrote exactly this row), no marker.
	if err := ledger.Append(ctx, billing.LedgerEntry{
		OrgID:  "org-a",
		Kind:   billing.KindUsageDrawdown,
		Amount: -128,
		Key:    billing.DrawdownKey("org-a", "sb-1", w1),
		At:     now.Add(-9 * time.Minute),
		Note:   "usage drawdown",
	}); err != nil {
		t.Fatalf("legacy append: %v", err)
	}
	balBefore, _ := ledger.Balance(ctx, "org-a")

	orgs := &fakeOrgLister{orgs: []saas.Organization{{ID: "org-a"}}}
	store := &fakeRecordLister{records: map[string][]usage.UsageRecord{
		"org-a": {centRecord("org-a", "sb-1", w1)},
	}}
	stats := runDrawdownOnce(ctx, testLogger(), orgs, store, svc, 2*time.Hour, now, nil)

	if stats.drawn != 0 || stats.replayed != 1 || stats.settledCents != 0 {
		t.Fatalf("stats = %+v, want drawn=0 replayed=1 settledCents=0 (legacy-settled window must be skipped)", stats)
	}
	balAfter, _ := ledger.Balance(ctx, "org-a")
	if balAfter != balBefore {
		t.Errorf("balance moved on a legacy-settled window: %d -> %d", int64(balBefore), int64(balAfter))
	}
}
