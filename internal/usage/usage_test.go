package usage

import (
	"context"
	"testing"
	"time"

	"mitos.run/mitos/internal/metering"
)

const giB = 1 << 30

func at(base time.Time, sec int) time.Time { return base.Add(time.Duration(sec) * time.Second) }

// baseTime is window-aligned to keep the math obvious.
var baseTime = time.Date(2026, 6, 21, 12, 0, 0, 0, time.UTC)

// TestIntegrateRateUnits checks the left-rectangle integration of the rate units
// (vCPU-seconds, memory GiB-seconds, storage GiB-hours) over a single window.
func TestIntegrateRateUnits(t *testing.T) {
	// Two samples 30s apart in one 60s window. The level of the first sample holds
	// until the second. 2 vCPUs for 30s => 60 vCPU-seconds; 1 GiB memory for 30s
	// => 30 GiB-seconds; 2 GiB storage for 30s => 2*30/3600 GiB-hours.
	samples := []Sample{
		{OrgID: "orgA", SandboxID: "sbx1", Node: "n1", Timestamp: at(baseTime, 0), VCPUs: 2, MemUniqueBytes: giB, DiskBytes: 2 * giB},
		{OrgID: "orgA", SandboxID: "sbx1", Node: "n1", Timestamp: at(baseTime, 30), VCPUs: 2, MemUniqueBytes: giB, DiskBytes: 2 * giB},
	}
	recs := Integrate(samples, DefaultConfig())
	if len(recs) != 1 {
		t.Fatalf("want 1 record, got %d", len(recs))
	}
	r := recs[0]
	if r.VCPUSeconds != 60 {
		t.Errorf("VCPUSeconds = %v, want 60", r.VCPUSeconds)
	}
	if r.MemGiBSeconds != 30 {
		t.Errorf("MemGiBSeconds = %v, want 30", r.MemGiBSeconds)
	}
	wantStorage := 2.0 * 30.0 / 3600.0
	if !approx(r.StorageGiBHours, wantStorage) {
		t.Errorf("StorageGiBHours = %v, want %v", r.StorageGiBHours, wantStorage)
	}
}

// TestCounterUnitsAreDelta checks egress + GPU-seconds are read as a delta of the
// cumulative counter, not integrated.
func TestCounterUnitsAreDelta(t *testing.T) {
	samples := []Sample{
		{OrgID: "orgA", SandboxID: "sbx1", Timestamp: at(baseTime, 0), VCPUs: 1, EgressBytes: 100, GPUSeconds: 10},
		{OrgID: "orgA", SandboxID: "sbx1", Timestamp: at(baseTime, 30), VCPUs: 1, EgressBytes: 350, GPUSeconds: 40},
	}
	recs := Integrate(samples, DefaultConfig())
	if len(recs) != 1 {
		t.Fatalf("want 1 record, got %d", len(recs))
	}
	if recs[0].EgressBytes != 250 {
		t.Errorf("EgressBytes = %d, want 250", recs[0].EgressBytes)
	}
	if recs[0].GPUSeconds != 30 {
		t.Errorf("GPUSeconds = %d, want 30", recs[0].GPUSeconds)
	}
}

// TestWindowSplit checks samples spanning a window boundary produce one record
// per window, each integrated only over its own window.
func TestWindowSplit(t *testing.T) {
	// 0s, 60s, 120s: 1 vCPU held across two full windows.
	samples := []Sample{
		{OrgID: "orgA", SandboxID: "sbx1", Timestamp: at(baseTime, 0), VCPUs: 1},
		{OrgID: "orgA", SandboxID: "sbx1", Timestamp: at(baseTime, 60), VCPUs: 1},
		{OrgID: "orgA", SandboxID: "sbx1", Timestamp: at(baseTime, 120), VCPUs: 1},
	}
	recs := Integrate(samples, DefaultConfig())
	if len(recs) != 2 {
		t.Fatalf("want 2 records (two windows), got %d", len(recs))
	}
	for _, r := range recs {
		if r.VCPUSeconds != 60 {
			t.Errorf("window %v VCPUSeconds = %v, want 60", r.Window, r.VCPUSeconds)
		}
	}
}

// TestDuplicateSampleNoDoubleBill is the load-bearing idempotency property: a
// duplicate sample (same sandbox, same timestamp) must not inflate the integral.
func TestDuplicateSampleNoDoubleBill(t *testing.T) {
	clean := []Sample{
		{OrgID: "orgA", SandboxID: "sbx1", Timestamp: at(baseTime, 0), VCPUs: 2},
		{OrgID: "orgA", SandboxID: "sbx1", Timestamp: at(baseTime, 30), VCPUs: 2},
	}
	withDup := []Sample{
		clean[0], clean[0], // exact duplicate of the first sample
		clean[1],
	}
	a := Integrate(clean, DefaultConfig())
	b := Integrate(withDup, DefaultConfig())
	if len(a) != 1 || len(b) != 1 {
		t.Fatalf("want 1 record each, got %d and %d", len(a), len(b))
	}
	if a[0].VCPUSeconds != b[0].VCPUSeconds {
		t.Errorf("duplicate changed the bill: clean=%v dup=%v", a[0].VCPUSeconds, b[0].VCPUSeconds)
	}
}

// TestMissedScrapeHoldThenGap checks the documented missed-scrape decision:
// hold the earlier level for MaxHold, then gap (zero) for the remainder.
func TestMissedScrapeHoldThenGap(t *testing.T) {
	cfg := DefaultConfig()
	// Put both samples in ONE window so the gap is wholly inside the window. Window
	// is 60s here; widen it so a long hold fits. Use a 1h window and MaxHold 10s.
	cfg.Window = time.Hour
	cfg.MaxHold = 10 * time.Second
	samples := []Sample{
		{OrgID: "orgA", SandboxID: "sbx1", Timestamp: at(baseTime, 0), VCPUs: 1},
		{OrgID: "orgA", SandboxID: "sbx1", Timestamp: at(baseTime, 100), VCPUs: 1},
	}
	recs := Integrate(samples, cfg)
	if len(recs) != 1 {
		t.Fatalf("want 1 record, got %d", len(recs))
	}
	// 100s gap, MaxHold 10s => only 10 vCPU-seconds billed, not 100.
	if recs[0].VCPUSeconds != 10 {
		t.Errorf("hold-then-gap: VCPUSeconds = %v, want 10 (held %v, gapped the rest)", recs[0].VCPUSeconds, cfg.MaxHold)
	}
}

// TestCrossNodeAggregation checks samples for two sandboxes on two nodes roll up
// into per-sandbox records and an org total summed across nodes.
func TestCrossNodeAggregation(t *testing.T) {
	samples := []Sample{
		{OrgID: "orgA", SandboxID: "sbx1", Node: "n1", Timestamp: at(baseTime, 0), VCPUs: 1},
		{OrgID: "orgA", SandboxID: "sbx1", Node: "n1", Timestamp: at(baseTime, 60), VCPUs: 1},
		{OrgID: "orgA", SandboxID: "sbx2", Node: "n2", Timestamp: at(baseTime, 0), VCPUs: 2},
		{OrgID: "orgA", SandboxID: "sbx2", Node: "n2", Timestamp: at(baseTime, 60), VCPUs: 2},
	}
	recs := Integrate(samples, DefaultConfig())
	var total float64
	seen := map[string]bool{}
	for _, r := range recs {
		total += r.VCPUSeconds
		seen[r.SandboxID] = true
	}
	if !seen["sbx1"] || !seen["sbx2"] {
		t.Fatalf("missing a sandbox: %v", seen)
	}
	// sbx1: 1 vCPU * 60s = 60; sbx2: 2 vCPU * 60s = 120; total 180.
	if total != 180 {
		t.Errorf("cross-node org total VCPUSeconds = %v, want 180", total)
	}
}

// TestSamplesFromReportCoWNoDoubleCount is the CoW-double-count guard: converting
// a node report to per-sandbox samples must amortize each template's shared-once
// set across its forks so the summed memory level reconstructs UsedCoWAware, not
// UsedNaive.
func TestSamplesFromReportCoWNoDoubleCount(t *testing.T) {
	// Two forks of one template: each 100 unique, 1000 shared. CoW-aware memory is
	// 200 (unique) + 1000 (shared once) = 1200. Naive is 200 + 2000 = 2200.
	report := metering.Aggregate([]metering.Sample{
		{ID: "sbx1", Template: "tpl", MemoryUnique: 100, MemoryShared: 1000},
		{ID: "sbx2", Template: "tpl", MemoryUnique: 100, MemoryShared: 1000},
	})
	if report.UsedCoWAware != 1200 {
		t.Fatalf("precondition: UsedCoWAware = %d, want 1200", report.UsedCoWAware)
	}

	orgOf := func(string) (string, bool) { return "orgA", true }
	samples, recon := SamplesFromReport("n1", baseTime, report, orgOf, func(string) int32 { return 1 })

	var sumLevel int64
	for _, s := range samples {
		sumLevel += s.MemUniqueBytes + s.MemSharedAmortizedBytes
	}
	if sumLevel != report.UsedCoWAware {
		t.Errorf("summed billable memory level = %d, want UsedCoWAware %d (must not be UsedNaive %d)",
			sumLevel, report.UsedCoWAware, report.UsedNaive)
	}
	// Audit reconciliation must keep both figures visible.
	if recon.CoWAware != report.UsedCoWAware || recon.Naive != report.UsedNaive {
		t.Errorf("reconciliation = %+v, want CoWAware %d Naive %d", recon, report.UsedCoWAware, report.UsedNaive)
	}
	if recon.CoWSavings != report.UsedNaive-report.UsedCoWAware {
		t.Errorf("reconciliation CoWSavings = %d, want %d", recon.CoWSavings, report.UsedNaive-report.UsedCoWAware)
	}
}

// TestStoreUpsertIdempotent checks that re-upserting a record for the same
// (org, sandbox, window) key REPLACES, never adds.
func TestStoreUpsertIdempotent(t *testing.T) {
	ctx := context.Background()
	store := NewMemUsageStore()
	rec := UsageRecord{OrgID: "orgA", SandboxID: "sbx1", Window: baseTime, VCPUSeconds: 60}
	if err := store.UpsertRecord(ctx, rec); err != nil {
		t.Fatal(err)
	}
	// Re-upsert the same key with a recomputed (larger) value: must replace.
	rec.VCPUSeconds = 90
	if err := store.UpsertRecord(ctx, rec); err != nil {
		t.Fatal(err)
	}
	got, err := store.ListRecords(ctx, "orgA", time.Time{}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 record after re-upsert, got %d", len(got))
	}
	if got[0].VCPUSeconds != 90 {
		t.Errorf("upsert did not replace: VCPUSeconds = %v, want 90", got[0].VCPUSeconds)
	}
}

// TestRunCollectorIdempotentReplay drives the collector twice over the same
// overlapping samples and asserts the store holds the same records (no double
// bill) after the second pass: the end-to-end idempotency on (sandbox, window).
func TestRunCollectorIdempotentReplay(t *testing.T) {
	ctx := context.Background()
	store := NewMemUsageStore()
	samples := []Sample{
		{OrgID: "orgA", SandboxID: "sbx1", Timestamp: at(baseTime, 0), VCPUs: 2},
		{OrgID: "orgA", SandboxID: "sbx1", Timestamp: at(baseTime, 30), VCPUs: 2},
		{OrgID: "orgA", SandboxID: "sbx1", Timestamp: at(baseTime, 60), VCPUs: 2},
	}
	src := &staticSource{samples: samples}
	c := NewCollector(src, store, DefaultConfig())

	if err := c.CollectOnce(ctx); err != nil {
		t.Fatal(err)
	}
	first, _ := store.ListRecords(ctx, "orgA", time.Time{}, time.Time{})

	// Replay the exact same samples (duplicate scrape / restart): records must not
	// change.
	if err := c.CollectOnce(ctx); err != nil {
		t.Fatal(err)
	}
	second, _ := store.ListRecords(ctx, "orgA", time.Time{}, time.Time{})

	if len(first) != len(second) {
		t.Fatalf("replay changed record count: %d then %d", len(first), len(second))
	}
	for i := range first {
		if first[i].VCPUSeconds != second[i].VCPUSeconds {
			t.Errorf("replay double-billed window %v: %v then %v", first[i].Window, first[i].VCPUSeconds, second[i].VCPUSeconds)
		}
	}
}

func approx(a, b float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d < 1e-9
}

type staticSource struct{ samples []Sample }

func (s *staticSource) Collect(_ context.Context) ([]Sample, error) { return s.samples, nil }
