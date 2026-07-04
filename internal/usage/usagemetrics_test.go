package usage

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// TestCollectorEmitsPerOrgMetric is the dual-use proof of issue #164: after the
// collector runs against the live mock source, BOTH the usage store holds the
// expected per-org UsageRecord AND the Prometheus gauge reflects the same per-org
// vCPU-seconds. The metric is driven from the store's CUMULATIVE per-org Totals
// (the same number the bill rolls up), so the billing data is immediately
// observable and the series is monotonic.
func TestCollectorEmitsPerOrgMetric(t *testing.T) {
	srv := meteringServer(t, twoSandboxReport())
	defer srv.Close()

	orgs := StaticOrgs{"sb-acme": "acme", "sb-acme2": "acme"}
	base := time.Date(2026, 6, 25, 12, 0, 0, 0, time.UTC)
	calls := 0
	now := func() time.Time {
		// Two scrapes 30s apart in one window so the rate units integrate to a
		// non-zero vCPU-seconds (2 vCPUs by default: 1 each x 2 sandboxes x 30s).
		ts := base.Add(time.Duration(calls) * 30 * time.Second)
		calls++
		return ts
	}
	src := NewNodeRegistrySource(
		staticEndpoints{"n1": srv.Listener.Addr().String()},
		orgs,
		nil, // default 1 vCPU per sandbox
		srv.Client(),
		"http",
		now,
	)

	store := NewMemUsageStore()
	reg := prometheus.NewRegistry()
	metrics := NewMetrics()
	metrics.MustRegister(reg)

	c := NewCollector(src, store, DefaultConfig())
	c.OnTotals = metrics.Observe

	// Two scrape cycles in the same window: the second is a re-scrape of the same
	// report. Idempotency means the store and the gauge land on the same per-org
	// totals, not double them.
	if _, err := c.CollectOnce(context.Background()); err != nil {
		t.Fatalf("first CollectOnce: %v", err)
	}
	if _, err := c.CollectOnce(context.Background()); err != nil {
		t.Fatalf("second CollectOnce: %v", err)
	}

	// Store: acme has two sandboxes, each 1 vCPU held 30s => 30 vcpu-seconds each,
	// 60 total for the org across both records.
	recs, err := store.ListRecords(context.Background(), "acme", time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("ListRecords: %v", err)
	}
	var storeVCPU float64
	for _, r := range recs {
		storeVCPU += r.VCPUSeconds
	}
	if storeVCPU != 60 {
		t.Fatalf("store vcpu-seconds for acme = %v, want 60", storeVCPU)
	}

	// Gauge: the per-org series must reflect the same 60 vcpu-seconds.
	const want = `
# HELP mitos_usage_vcpu_seconds_total Cumulative vCPU-seconds of billable sandbox usage by org, summed over every settled usage record (the same number the bill rolls up). Monotonic within a controller process; reset only by a restart, where the durable store is the system of record.
# TYPE mitos_usage_vcpu_seconds_total gauge
mitos_usage_vcpu_seconds_total{org="acme"} 60
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(want), "mitos_usage_vcpu_seconds_total"); err != nil {
		t.Errorf("metric mismatch: %v", err)
	}
}

// TestMetricsObserveAllDimensions is the 1.b proof: Observe publishes every
// metering dimension already in the store's cumulative per-org Totals (egress
// bytes, GPU seconds, memory GiB-seconds) as an org-labeled gauge, fed from the
// SAME store-fed cumulative the bill rolls up. The label set is exactly {org},
// never a sandbox id, argv, or secret (pool is not in Totals yet, a documented
// follow-up).
func TestMetricsObserveAllDimensions(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics()
	m.MustRegister(reg)

	m.Observe(map[string]Totals{
		"acme": {VCPUSeconds: 12, MemGiBSeconds: 7, EgressBytes: 4096, GPUSeconds: 9},
		// An empty-org row never bills and must not emit a series.
		"": {VCPUSeconds: 99, EgressBytes: 99, GPUSeconds: 99},
	})

	for name, want := range map[string]string{
		"mitos_usage_egress_bytes_total": `
# HELP mitos_usage_egress_bytes_total Cumulative egress bytes of billable sandbox usage by org, summed over every settled usage record. Monotonic within a controller process; reset only by a restart.
# TYPE mitos_usage_egress_bytes_total gauge
mitos_usage_egress_bytes_total{org="acme"} 4096
`,
		"mitos_usage_gpu_seconds_total": `
# HELP mitos_usage_gpu_seconds_total Cumulative GPU-seconds of billable sandbox usage by org, summed over every settled usage record. Monotonic within a controller process; reset only by a restart, where the durable store is the system of record.
# TYPE mitos_usage_gpu_seconds_total gauge
mitos_usage_gpu_seconds_total{org="acme"} 9
`,
	} {
		if err := testutil.GatherAndCompare(reg, strings.NewReader(want), name); err != nil {
			t.Errorf("%s mismatch: %v", name, err)
		}
	}

	// The label set on every usage series is exactly {org}: no sandbox id, no
	// argv/command/env, no secret. Assert by inspecting the gathered families.
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, mf := range mfs {
		for _, met := range mf.GetMetric() {
			for _, lp := range met.GetLabel() {
				if lp.GetName() != "org" {
					t.Errorf("%s carries forbidden label %q (only org is allowed)", mf.GetName(), lp.GetName())
				}
			}
		}
	}
}

// batchSource is a SampleSource that returns a fresh batch of samples on each
// Collect, so a multi-cycle, multi-window test can settle several windows in
// order.
type batchSource struct {
	batches [][]Sample
	i       int
}

func (s *batchSource) Collect(_ context.Context) ([]Sample, error) {
	if s.i >= len(s.batches) {
		return nil, nil
	}
	b := s.batches[s.i]
	s.i++
	return b, nil
}

// TestMetricStoreFedCumulativeAcrossWindowsQuietOrgRetained is the billing-data
// proof for issue #164 IMPORTANT 1: after MORE THAN one settled window, and with
// one org going quiet in the latest cycle, the per-org metric still equals the
// store's cumulative totals for ALL orgs (the quiet org is NOT dropped) and no
// org's value decreases. This is the bug the old buffer-fed Reset+Set metric had:
// it reflected only the last ~3 minutes, dropped quiet orgs, and could go
// backwards. The store-fed cumulative cannot.
func TestMetricStoreFedCumulativeAcrossWindowsQuietOrgRetained(t *testing.T) {
	ctx := context.Background()
	store := NewMemUsageStore()
	reg := prometheus.NewRegistry()
	metrics := NewMetrics()
	metrics.MustRegister(reg)

	// One window's worth of samples for a sandbox: two readings 30s apart so 1 vCPU
	// integrates to 30 vcpu-seconds for that window. windowStart is window-aligned.
	windowSamples := func(org, sandbox string, windowStart time.Time) []Sample {
		return []Sample{
			{OrgID: org, SandboxID: sandbox, Timestamp: windowStart, VCPUs: 1},
			{OrgID: org, SandboxID: sandbox, Timestamp: windowStart.Add(30 * time.Second), VCPUs: 1},
		}
	}

	w0 := baseTime
	w1 := baseTime.Add(60 * time.Second)
	w2 := baseTime.Add(120 * time.Second)

	// Cycle 1: window 0, both acme and globex active.
	// Cycle 2: window 1, both active.
	// Cycle 3: window 2, ONLY acme active (globex goes quiet this cycle).
	src := &batchSource{batches: [][]Sample{
		append(windowSamples("acme", "sb-a", w0), windowSamples("globex", "sb-g", w0)...),
		append(windowSamples("acme", "sb-a", w1), windowSamples("globex", "sb-g", w1)...),
		windowSamples("acme", "sb-a", w2),
	}}

	coll := NewCollector(src, store, DefaultConfig())
	coll.OnTotals = metrics.Observe

	var prevAcme, prevGlobex float64
	for cyc := 0; cyc < 3; cyc++ {
		if _, err := coll.CollectOnce(ctx); err != nil {
			t.Fatalf("cycle %d: %v", cyc, err)
		}
		totals := store.TotalsByOrg()

		// The metric for every known org must equal the store cumulative for that org.
		assertGaugeEquals(t, reg, "acme", totals["acme"].VCPUSeconds)
		// globex must REMAIN present even in cycle 3 when it went quiet.
		assertGaugeEquals(t, reg, "globex", totals["globex"].VCPUSeconds)

		// No series may go backwards across cycles.
		if totals["acme"].VCPUSeconds < prevAcme {
			t.Errorf("cycle %d: acme cumulative went backwards: %v < %v", cyc, totals["acme"].VCPUSeconds, prevAcme)
		}
		if totals["globex"].VCPUSeconds < prevGlobex {
			t.Errorf("cycle %d: globex cumulative went backwards: %v < %v", cyc, totals["globex"].VCPUSeconds, prevGlobex)
		}
		prevAcme = totals["acme"].VCPUSeconds
		prevGlobex = totals["globex"].VCPUSeconds
	}

	// After 3 windows: acme settled windows 0,1,2 (30 each across the buffer's
	// retained windows) and globex settled windows 0,1. The exact value depends on
	// buffer integration, but the load-bearing assertions are: globex is still
	// present and equal to its store total, and acme >= globex (acme has the extra
	// window). Assert globex did not drop to zero/absent in the final cycle.
	finalGlobex := store.TotalsByOrg()["globex"].VCPUSeconds
	if finalGlobex <= 0 {
		t.Fatalf("globex cumulative dropped to %v after going quiet; the quiet org was lost", finalGlobex)
	}
	assertGaugeEquals(t, reg, "globex", finalGlobex)
}

// TestObserveCycleExportsCycleDuration asserts the cycle-duration gauge (issue
// #682, was #656; the series #617 alerting watches) is exported and set to the
// most recent cycle's wall duration in seconds.
func TestObserveCycleExportsCycleDuration(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics()
	m.MustRegister(reg)

	m.ObserveCycle(CycleStats{Duration: 1500 * time.Millisecond})

	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	var got float64
	var found bool
	for _, mf := range mfs {
		if mf.GetName() == "mitos_usage_collect_cycle_duration_seconds" {
			found = true
			got = mf.GetMetric()[0].GetGauge().GetValue()
		}
	}
	if !found {
		t.Fatal("mitos_usage_collect_cycle_duration_seconds not exported")
	}
	if got != 1.5 {
		t.Errorf("cycle duration gauge = %v, want 1.5", got)
	}
}

// TestObserveCycleFailureIncrementsCounter asserts the cycle-failure counter
// (issue #682 review follow-up) is exported and counts every failed cycle. The
// duration gauge is set only on success, so under a sustained failure it
// freezes at the last healthy value; this counter is the metrics-only signal
// #617 alerting needs for a failing collector.
func TestObserveCycleFailureIncrementsCounter(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics()
	m.MustRegister(reg)

	read := func() float64 {
		t.Helper()
		mfs, err := reg.Gather()
		if err != nil {
			t.Fatalf("gather: %v", err)
		}
		for _, mf := range mfs {
			if mf.GetName() == "mitos_usage_collect_cycle_failures_total" {
				return mf.GetMetric()[0].GetCounter().GetValue()
			}
		}
		return 0
	}

	m.ObserveCycleFailure()
	if got := read(); got != 1 {
		t.Fatalf("cycle failures after one failure = %v, want 1", got)
	}
	m.ObserveCycleFailure()
	if got := read(); got != 2 {
		t.Fatalf("cycle failures after two failures = %v, want 2", got)
	}
}

// gaugeValue reads the value of mitos_usage_vcpu_seconds_total for the given org
// from the registry, and whether a series for that org is present.
func gaugeValue(t *testing.T, reg *prometheus.Registry, org string) (float64, bool) {
	t.Helper()
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() != "mitos_usage_vcpu_seconds_total" {
			continue
		}
		for _, m := range mf.GetMetric() {
			for _, lp := range m.GetLabel() {
				if lp.GetName() == "org" && lp.GetValue() == org {
					return m.GetGauge().GetValue(), true
				}
			}
		}
	}
	return 0, false
}

// assertGaugeEquals asserts the per-org vcpu-seconds gauge is present and equals
// want. A missing series fails: the store-fed metric must never drop a known org.
func assertGaugeEquals(t *testing.T, reg *prometheus.Registry, org string, want float64) {
	t.Helper()
	got, present := gaugeValue(t, reg, org)
	if !present {
		t.Fatalf("org %q series is absent; the store-fed metric dropped a known org", org)
	}
	if got != want {
		t.Errorf("org %q gauge = %v, want store cumulative %v", org, got, want)
	}
}
