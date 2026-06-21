package benchstat

import (
	"bytes"
	"testing"
	"time"
)

func ms2(n int) time.Duration { return time.Duration(n) * time.Millisecond }

// TestAggregatePrefetchFaultDelta proves the aggregation reports the fault-count
// reduction prefetch buys: the headline #167 number is faults per resume on vs
// off, so the delta and the ratio must come straight from the recorded counts.
func TestAggregatePrefetchFaultDelta(t *testing.T) {
	off := PrefetchArm{
		FaultCounts: []int{100000, 98000, 102000},
		ClaimToExec: []time.Duration{ms2(40), ms2(42), ms2(38)},
	}
	on := PrefetchArm{
		FaultCounts: []int{1100, 1000, 1200},
		ClaimToExec: []time.Duration{ms2(12), ms2(11), ms2(13)},
	}
	cmp := AggregatePrefetch(off, on)

	if cmp.Off.MeanFaults != 100000 {
		t.Fatalf("off mean faults: want 100000, got %d", cmp.Off.MeanFaults)
	}
	if cmp.On.MeanFaults != 1100 {
		t.Fatalf("on mean faults: want 1100, got %d", cmp.On.MeanFaults)
	}
	if cmp.FaultReduction != 100000-1100 {
		t.Fatalf("fault reduction: want %d, got %d", 100000-1100, cmp.FaultReduction)
	}
	// Latency P50 must be drawn from the on/off claim->exec distributions.
	if cmp.Off.ClaimToExec.P50 != ms2(40) {
		t.Fatalf("off p50: want 40ms, got %s", cmp.Off.ClaimToExec.P50)
	}
	if cmp.On.ClaimToExec.P50 != ms2(12) {
		t.Fatalf("on p50: want 12ms, got %s", cmp.On.ClaimToExec.P50)
	}
}

// TestAggregatePrefetchEmptyArms proves an unmeasured arm yields zeroed stats
// rather than panicking: on a host that could not run one side, the aggregation
// must still produce a well-formed comparison.
func TestAggregatePrefetchEmptyArms(t *testing.T) {
	cmp := AggregatePrefetch(PrefetchArm{}, PrefetchArm{})
	if cmp.Off.MeanFaults != 0 || cmp.On.MeanFaults != 0 || cmp.FaultReduction != 0 {
		t.Fatalf("empty arms produced non-zero stats: %+v", cmp)
	}
}

// TestPrefetchComparisonJSONRoundTrip proves the comparison serializes
// losslessly: the bench driver writes it as JSON for the CI assertions, so the
// fault counts and the nanosecond latency distributions must survive the round
// trip.
func TestPrefetchComparisonJSONRoundTrip(t *testing.T) {
	cmp := AggregatePrefetch(
		PrefetchArm{FaultCounts: []int{100, 200}, ClaimToExec: []time.Duration{ms2(40), ms2(42)}},
		PrefetchArm{FaultCounts: []int{10, 20}, ClaimToExec: []time.Duration{ms2(12), ms2(13)}},
	)
	var buf bytes.Buffer
	if err := WritePrefetchJSON(&buf, cmp); err != nil {
		t.Fatalf("WritePrefetchJSON: %v", err)
	}
	got, err := ReadPrefetchJSON(buf.Bytes())
	if err != nil {
		t.Fatalf("ReadPrefetchJSON: %v", err)
	}
	if got.FaultReduction != cmp.FaultReduction {
		t.Fatalf("fault reduction round-trip: want %d got %d", cmp.FaultReduction, got.FaultReduction)
	}
	if got.On.ClaimToExec.P50 != cmp.On.ClaimToExec.P50 {
		t.Fatalf("on p50 round-trip: want %s got %s", cmp.On.ClaimToExec.P50, got.On.ClaimToExec.P50)
	}
	if got.Off.MeanFaults != cmp.Off.MeanFaults {
		t.Fatalf("off mean faults round-trip: want %d got %d", cmp.Off.MeanFaults, got.Off.MeanFaults)
	}
}
