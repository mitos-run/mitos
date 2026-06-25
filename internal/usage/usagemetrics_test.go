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
// collector runs one cycle against the live mock source, BOTH the usage store
// holds the expected per-org UsageRecord AND the Prometheus gauge reflects the
// same per-org vCPU-seconds. The metric is derived from the SAME integrated
// records the store receives, so the billing data is immediately observable.
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
	c.OnRecords = metrics.Observe

	// Two scrape cycles in the same window: the second is a re-scrape of the same
	// report. Idempotency means the store and the gauge land on the same per-org
	// totals, not double them.
	if err := c.CollectOnce(context.Background()); err != nil {
		t.Fatalf("first CollectOnce: %v", err)
	}
	if err := c.CollectOnce(context.Background()); err != nil {
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
# HELP mitos_usage_vcpu_seconds_total Integrated vCPU-seconds of billable sandbox usage, by org, as of the last collector cycle.
# TYPE mitos_usage_vcpu_seconds_total gauge
mitos_usage_vcpu_seconds_total{org="acme"} 60
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(want), "mitos_usage_vcpu_seconds_total"); err != nil {
		t.Errorf("metric mismatch: %v", err)
	}
}
