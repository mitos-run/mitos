package daemon

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/paperclipinc/mitos/internal/fork"
	"github.com/paperclipinc/mitos/internal/metering"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// gaugeValue reads the current value of a prometheus Gauge without pulling in the
// testutil helper (and its extra test-only dependency).
func gaugeValue(t *testing.T, g prometheus.Gauge) float64 {
	t.Helper()
	var m dto.Metric
	if err := g.Write(&m); err != nil {
		t.Fatalf("write gauge: %v", err)
	}
	return m.GetGauge().GetValue()
}

// meteringSpyEngine embeds a MockEngine (to satisfy the ForkEngine interface) but
// overrides Metering to return a fixed report and count how many times it was
// sampled, so a test can prove the periodic sampler actually drives UpdateMetrics.
type meteringSpyEngine struct {
	*fork.MockEngine
	unique int64
	calls  atomic.Int64
}

func (e *meteringSpyEngine) Metering() metering.Report {
	e.calls.Add(1)
	return metering.Report{TotalUnique: e.unique}
}

// TestUpdateMetricsPopulatesMemoryGauge proves UpdateMetrics pushes the engine's
// metering report into the mitos_memory_unique_bytes gauge. Before the periodic
// sampler was wired this gauge was never populated, so /metrics reported a stale
// zero (fork-correctness Row 5, issue #3).
func TestUpdateMetricsPopulatesMemoryGauge(t *testing.T) {
	const mib = int64(1024 * 1024)
	eng := &meteringSpyEngine{MockEngine: fork.NewMockEngine(), unique: 7 * mib}
	srv := NewServer(eng, NewSandboxAPI(t.TempDir()))

	srv.UpdateMetrics()

	if got := gaugeValue(t, memoryUnique); got != float64(7*mib) {
		t.Fatalf("mitos_memory_unique_bytes = %v, want %d", got, 7*mib)
	}
}

// TestSampleMetricsTicksAndStopsOnContextCancel proves the sampler refreshes the
// gauges periodically (the metering engine is sampled more than once) AND returns
// promptly when its context is cancelled, so it is a well-behaved background loop
// (fork-correctness Row 5, issue #3).
func TestSampleMetricsTicksAndStopsOnContextCancel(t *testing.T) {
	const mib = int64(1024 * 1024)
	eng := &meteringSpyEngine{MockEngine: fork.NewMockEngine(), unique: 3 * mib}
	srv := NewServer(eng, NewSandboxAPI(t.TempDir()))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		srv.SampleMetrics(ctx, 2*time.Millisecond)
		close(done)
	}()

	// The loop samples once immediately and then on each tick: wait for at least
	// two samples so we have proven it is periodic, not one-shot.
	deadline := time.After(2 * time.Second)
	for eng.calls.Load() < 2 {
		select {
		case <-deadline:
			t.Fatalf("sampler did not tick at least twice; calls=%d", eng.calls.Load())
		case <-time.After(time.Millisecond):
		}
	}

	if got := gaugeValue(t, memoryUnique); got != float64(3*mib) {
		t.Fatalf("mitos_memory_unique_bytes = %v, want %d", got, 3*mib)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("SampleMetrics did not return after context cancel")
	}
}
