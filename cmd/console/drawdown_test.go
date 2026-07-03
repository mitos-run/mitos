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
}

func (f *fakeDrawdowner) Drawdown(_ context.Context, rec usage.UsageRecord) (billing.DrawdownResult, error) {
	key := rec.OrgID + "|" + rec.SandboxID + "|" + rec.Window.UTC().Format(time.RFC3339)
	f.keys = append(f.keys, key)
	return f.results[key], f.err
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

	stats := runDrawdownOnce(context.Background(), testLogger(), orgs, store, svc, 2*time.Hour, now)

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

	stats := runDrawdownOnce(context.Background(), testLogger(), orgs, store, svc, time.Hour, now)

	if len(svc.keys) != 2 {
		t.Fatalf("Drawdown called %d times, want 2 (a failed drawdown must not abort the cycle)", len(svc.keys))
	}
	if stats.drawn != 0 || stats.failed != 2 {
		t.Errorf("stats = %+v, want drawn=0 failed=2", stats)
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

	stats := runDrawdownOnce(context.Background(), testLogger(), orgs, store, svc, 2*time.Hour, now)

	if stats.settledCents != 3 {
		t.Errorf("settledCents = %d, want 3 (1 from org-a + 2 from org-b)", stats.settledCents)
	}
	// Per org, the LAST settled record's carried remainder is the org's current
	// remainder; the stat is the sum across orgs: 177 + (-300).
	if stats.carriedMilli != -123 {
		t.Errorf("carriedMilli = %d, want -123 (org-a 177 + org-b -300)", stats.carriedMilli)
	}
}
