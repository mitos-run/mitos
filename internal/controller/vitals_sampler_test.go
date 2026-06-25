package controller

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// mkEntry builds a scraped node-vitals entry with n processes so the
// process_count signal is exactly n, without naming any process.
func mkEntry(sandboxID, pool string, steal float64, balloonKB, usedKB uint64, n int) scrapedVitalsEntry {
	var e scrapedVitalsEntry
	e.SandboxID = sandboxID
	e.Pool = pool
	e.Vitals.StealFraction = steal
	e.Vitals.BalloonReclaimedKB = balloonKB
	e.Vitals.MemUsedKB = usedKB
	e.Vitals.ProcessCount = n // the numeric count is the only process signal
	return e
}

// TestAggregateVitals_MaxStealSumMem proves the documented aggregation: cpu_steal
// is the MAX across an (org, pool) bucket (the worst-starved sandbox), while
// balloon/used memory and process_count are SUMs (the additive fleet footprint).
// A sandbox with no resolvable org is SKIPPED and counted, never attributed.
func TestAggregateVitals_MaxStealSumMem(t *testing.T) {
	entries := []scrapedVitalsEntry{
		// acme/pool-x: two sandboxes. steal max(0.2,0.5)=0.5 -> 50%. mem sums.
		mkEntry("sb-a1", "pool-x", 0.2, 100, 1000, 3),
		mkEntry("sb-a2", "pool-x", 0.5, 200, 2000, 4),
		// acme/pool-y: distinct bucket from pool-x.
		mkEntry("sb-a3", "pool-y", 0.1, 50, 500, 1),
		// no-org sandbox: must be skipped, never attributed.
		mkEntry("sb-orphan", "pool-x", 0.9, 999, 999, 99),
	}
	orgFor := func(id string) (string, bool) {
		switch id {
		case "sb-a1", "sb-a2", "sb-a3":
			return "acme", true
		default:
			return "", false
		}
	}

	aggs, unattributed := aggregateVitals(entries, orgFor)
	if unattributed != 1 {
		t.Fatalf("unattributed = %d, want 1 (the orphan)", unattributed)
	}

	x := aggs[vitalsKey{org: "acme", pool: "pool-x"}]
	if x.maxStealPercent != 50 {
		t.Errorf("pool-x max steal = %v, want 50 (max, not sum/avg)", x.maxStealPercent)
	}
	if x.sumBalloonBytes != (100+200)*1024 {
		t.Errorf("pool-x balloon = %v, want sum %v", x.sumBalloonBytes, (100+200)*1024)
	}
	if x.sumUsedBytes != (1000+2000)*1024 {
		t.Errorf("pool-x used = %v, want sum %v", x.sumUsedBytes, (1000+2000)*1024)
	}
	if x.sumProcessCount != 3+4 {
		t.Errorf("pool-x process_count = %v, want sum 7", x.sumProcessCount)
	}

	y := aggs[vitalsKey{org: "acme", pool: "pool-y"}]
	if y.maxStealPercent != 10 || y.sumProcessCount != 1 {
		t.Errorf("pool-y agg wrong: %+v", y)
	}

	// The orphan's huge values must not appear in any series.
	for k := range aggs {
		if k.org == "" {
			t.Errorf("empty-org series present: %+v", k)
		}
	}
}

// TestVitalsMetricsObserve_LabelSetAndValues proves the gauges reflect the
// aggregates labeled by exactly {org, pool}, with NO command/argv/env/pid label.
func TestVitalsMetricsObserve_LabelSetAndValues(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := newVitalsMetrics()
	m.mustRegister(reg)

	m.observe(map[vitalsKey]vitalsAgg{
		{org: "acme", pool: "pool-x"}: {maxStealPercent: 50, sumBalloonBytes: 307200, sumUsedBytes: 3072000, sumProcessCount: 7},
	})

	wantSteal := `
# HELP mitos_guest_cpu_steal_percent Max CPU steal percent (0-100) across the guests in this org and pool, sampled from forkd GET /v1/vitals/node. Max is the worst-starved sandbox in the bucket; the 'is my org's fleet starved' signal. Labels are org and pool only, never a sandbox id or secret.
# TYPE mitos_guest_cpu_steal_percent gauge
mitos_guest_cpu_steal_percent{org="acme",pool="pool-x"} 50
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(wantSteal), "mitos_guest_cpu_steal_percent"); err != nil {
		t.Errorf("steal gauge mismatch: %v", err)
	}
	wantProc := `
# HELP mitos_guest_process_count Sum of the in-guest process count (the LENGTH of each guest's process table, a number) across the guests in this org and pool. No process command, argv, pid, or env is ever exported. Labels are org and pool only.
# TYPE mitos_guest_process_count gauge
mitos_guest_process_count{org="acme",pool="pool-x"} 7
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(wantProc), "mitos_guest_process_count"); err != nil {
		t.Errorf("process_count gauge mismatch: %v", err)
	}

	// The label set on EVERY guest series is exactly {org, pool}: no command,
	// argv, pid, env, or sandbox-id label is permitted.
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, mf := range mfs {
		for _, met := range mf.GetMetric() {
			names := map[string]bool{}
			for _, lp := range met.GetLabel() {
				names[lp.GetName()] = true
				if lp.GetName() != "org" && lp.GetName() != "pool" {
					t.Errorf("%s carries forbidden label %q (only org,pool allowed)", mf.GetName(), lp.GetName())
				}
			}
			if !names["org"] || !names["pool"] {
				t.Errorf("%s label set %v, want exactly {org,pool}", mf.GetName(), names)
			}
		}
	}
}

// TestVitalsMetricsObserve_ResetDropsStaleSeries proves the instantaneous gauges
// drop a bucket that has no guests in the latest cycle (Reset+Set), unlike the
// monotonic store-fed usage cumulative.
func TestVitalsMetricsObserve_ResetDropsStaleSeries(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := newVitalsMetrics()
	m.mustRegister(reg)

	m.observe(map[vitalsKey]vitalsAgg{{org: "acme", pool: "pool-x"}: {sumProcessCount: 5}})
	if got := testutil.ToFloat64(m.processCount.WithLabelValues("acme", "pool-x")); got != 5 {
		t.Fatalf("first cycle process_count = %v, want 5", got)
	}
	// Next cycle: acme/pool-x has no guests; only globex/pool-z reports.
	m.observe(map[vitalsKey]vitalsAgg{{org: "globex", pool: "pool-z"}: {sumProcessCount: 2}})

	mfs, _ := reg.Gather()
	for _, mf := range mfs {
		if mf.GetName() != "mitos_guest_process_count" {
			continue
		}
		for _, met := range mf.GetMetric() {
			for _, lp := range met.GetLabel() {
				if lp.GetName() == "org" && lp.GetValue() == "acme" {
					t.Errorf("stale acme series survived a cycle with no acme guests")
				}
			}
		}
	}
}

// TestVitalsScrape_DecodesNodeReport proves the scraper decodes forkd's
// /v1/vitals/node JSON wire shape (sandbox_id, pool, numeric vitals, numeric
// process_count) and the unreachable-guest skip count, against an httptest server.
// The node endpoint sends a numeric process_count, never a per-process table.
func TestVitalsScrape_DecodesNodeReport(t *testing.T) {
	const body = `{"sandboxes":[{"sandbox_id":"sb-a1","pool":"pool-x","vitals":{"steal_fraction":0.25,"mem_used_kb":1000,"balloon_reclaimed_kb":100,"process_count":2}}],"skipped":3}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != nodeVitalsPath {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	s := &VitalsSamplerRunnable{}
	nv, ok := s.scrape(context.Background(), srv.Client(), "http", strings.TrimPrefix(srv.URL, "http://"))
	if !ok {
		t.Fatal("scrape returned ok=false for a healthy node")
	}
	if nv.Skipped != 3 {
		t.Errorf("Skipped = %d, want 3", nv.Skipped)
	}
	if len(nv.Sandboxes) != 1 {
		t.Fatalf("want 1 sandbox, got %d", len(nv.Sandboxes))
	}
	e := nv.Sandboxes[0]
	if e.SandboxID != "sb-a1" || e.Pool != "pool-x" {
		t.Errorf("ids/pool wrong: %+v", e)
	}
	if e.Vitals.StealFraction != 0.25 || e.Vitals.MemUsedKB != 1000 {
		t.Errorf("vitals wrong: %+v", e.Vitals)
	}
	if e.Vitals.ProcessCount != 2 {
		t.Errorf("process_count = %d, want 2 (numeric count only, no command read)", e.Vitals.ProcessCount)
	}
}

// TestVitalsScrape_UnreachableNodeSkipped proves a node that is down yields
// ok=false, so the cycle skips+counts it instead of failing.
func TestVitalsScrape_UnreachableNodeSkipped(t *testing.T) {
	s := &VitalsSamplerRunnable{}
	// 127.0.0.1:1 has nothing listening; the scrape must fail closed.
	_, ok := s.scrape(context.Background(), &http.Client{}, "http", "127.0.0.1:1")
	if ok {
		t.Error("scrape returned ok=true for an unreachable node")
	}
}
