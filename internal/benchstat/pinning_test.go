package benchstat

import (
	"bytes"
	"testing"
	"time"
)

// TestAggregatePinningSuccessRate proves the aggregation reports the activate
// success rate and latency distribution per arm, and the success-rate lift
// pinning buys. The headline #168 numbers are activate success under a claim
// storm (pinning on vs off) and the activate latency distribution.
func TestAggregatePinningSuccessRate(t *testing.T) {
	off := PinningArm{
		Outcomes: []ActivateOutcome{
			{OK: true, Latency: ms2(40)},
			{OK: false},
			{OK: true, Latency: ms2(50)},
			{OK: true, Latency: ms2(45)},
			{OK: false},
		},
	}
	on := PinningArm{
		Outcomes: []ActivateOutcome{
			{OK: true, Latency: ms2(30)},
			{OK: true, Latency: ms2(33)},
			{OK: true, Latency: ms2(31)},
			{OK: true, Latency: ms2(32)},
			{OK: true, Latency: ms2(34)},
		},
	}
	cmp := AggregatePinning(off, on)

	if cmp.Off.Activations != 5 || cmp.Off.Succeeded != 3 {
		t.Fatalf("off counts: %+v", cmp.Off)
	}
	if cmp.Off.SuccessRate != 0.6 {
		t.Fatalf("off success rate: want 0.6, got %v", cmp.Off.SuccessRate)
	}
	if cmp.On.SuccessRate != 1.0 {
		t.Fatalf("on success rate: want 1.0, got %v", cmp.On.SuccessRate)
	}
	// Lift is on - off success rate.
	if cmp.SuccessRateLift < 0.399 || cmp.SuccessRateLift > 0.401 {
		t.Fatalf("success-rate lift: want ~0.4, got %v", cmp.SuccessRateLift)
	}
	// Latency distribution is over the SUCCESSFUL activations only.
	if cmp.Off.Latency.Count != 3 {
		t.Fatalf("off latency count: want 3 (successes only), got %d", cmp.Off.Latency.Count)
	}
	if cmp.On.Latency.P50 != ms2(32) {
		t.Fatalf("on p50: want 32ms, got %s", cmp.On.Latency.P50)
	}
}

// TestAggregatePinningEmptyArms proves an unmeasured arm yields zeroed stats
// (success rate 0, empty distribution) rather than a divide-by-zero panic.
func TestAggregatePinningEmptyArms(t *testing.T) {
	cmp := AggregatePinning(PinningArm{}, PinningArm{})
	if cmp.Off.Activations != 0 || cmp.Off.SuccessRate != 0 || cmp.SuccessRateLift != 0 {
		t.Fatalf("empty arms produced non-zero stats: %+v", cmp)
	}
}

// TestPinningComparisonJSONRoundTrip proves the comparison serializes losslessly
// for the CI assertions: success rates, counts, and the nanosecond latency
// distribution must survive the round trip.
func TestPinningComparisonJSONRoundTrip(t *testing.T) {
	cmp := AggregatePinning(
		PinningArm{Outcomes: []ActivateOutcome{{OK: true, Latency: ms2(40)}, {OK: false}}},
		PinningArm{Outcomes: []ActivateOutcome{{OK: true, Latency: ms2(30)}, {OK: true, Latency: ms2(31)}}},
	)
	var buf bytes.Buffer
	if err := WritePinningJSON(&buf, cmp); err != nil {
		t.Fatalf("WritePinningJSON: %v", err)
	}
	got, err := ReadPinningJSON(buf.Bytes())
	if err != nil {
		t.Fatalf("ReadPinningJSON: %v", err)
	}
	if got.SuccessRateLift != cmp.SuccessRateLift {
		t.Fatalf("lift round-trip: want %v got %v", cmp.SuccessRateLift, got.SuccessRateLift)
	}
	if got.On.Latency.P50 != cmp.On.Latency.P50 {
		t.Fatalf("on p50 round-trip: want %s got %s", cmp.On.Latency.P50, got.On.Latency.P50)
	}
	if got.Off.Succeeded != cmp.Off.Succeeded {
		t.Fatalf("off succeeded round-trip: want %d got %d", cmp.Off.Succeeded, got.Off.Succeeded)
	}
}

// TestActivateOutcomeLatencyIgnoredOnFailure proves a failed activation's
// latency never pollutes the success-latency distribution.
func TestActivateOutcomeLatencyIgnoredOnFailure(t *testing.T) {
	arm := PinningArm{Outcomes: []ActivateOutcome{
		{OK: false, Latency: time.Hour}, // must be ignored
		{OK: true, Latency: ms2(10)},
	}}
	cmp := AggregatePinning(arm, PinningArm{})
	if cmp.Off.Latency.Count != 1 || cmp.Off.Latency.Max != ms2(10) {
		t.Fatalf("failed activation latency leaked into distribution: %+v", cmp.Off.Latency)
	}
}
