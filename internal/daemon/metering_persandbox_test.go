package daemon

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"mitos.run/mitos/internal/metering"
)

// countSeries returns the number of currently-exported series in a collector.
func countSeries(c prometheus.Collector) int {
	ch := make(chan prometheus.Metric)
	go func() { c.Collect(ch); close(ch) }()
	n := 0
	for range ch {
		n++
	}
	return n
}

// TestUpdateMetricsLabelsMemoryPerSandbox proves the lifetime memory metering is
// exported PER SANDBOX (issue #3 row 5), not only as a node-wide total: each
// sandbox in the metering report gets its own labeled series, and a sandbox that
// leaves the report (terminated) does not leave a stale series behind.
func TestUpdateMetricsLabelsMemoryPerSandbox(t *testing.T) {
	engine := &fakeMeteringEngine{report: sampleReport()}
	srv := NewServer(engine, nil)
	srv.UpdateMetrics()

	if n := countSeries(memoryUniquePerSandbox); n != 10 {
		t.Fatalf("per-sandbox memory series = %d, want 10 (one per sandbox in the report)", n)
	}
	g, err := memoryUniquePerSandbox.GetMetricWithLabelValues("a", "A")
	if err != nil {
		t.Fatal(err)
	}
	if got := readGauge(t, g); got != float64(1*mib) {
		t.Errorf("per-sandbox unique for sandbox a = %v, want %v", got, float64(1*mib))
	}

	// After every sandbox leaves the report, no stale per-sandbox series remain.
	engine.report = metering.Aggregate(nil)
	srv.UpdateMetrics()
	if n := countSeries(memoryUniquePerSandbox); n != 0 {
		t.Errorf("stale per-sandbox series after all sandboxes gone: got %d, want 0", n)
	}
}
