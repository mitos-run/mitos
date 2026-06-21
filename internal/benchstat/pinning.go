package benchstat

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
)

// ActivateOutcome is one activation under a claim storm (issue #168): whether it
// succeeded and, when it did, its activate latency. A failed activation's
// Latency is ignored, so a failure never pollutes the success-latency
// distribution.
type ActivateOutcome struct {
	OK      bool
	Latency time.Duration
}

// PinningArm is the raw observations from one side of a pinning-on-vs-off claim
// storm: the per-activation outcomes. AggregatePinning turns two arms into a
// PinningComparison. The outcomes are collected against the real KVM-backed
// engine under a real claim storm; off-KVM the bench driver never produces them,
// so no number is fabricated.
type PinningArm struct {
	Outcomes []ActivateOutcome
}

// PinArmStats is the aggregated view of one arm: the activation count, the
// success count and rate, and the latency distribution over the SUCCESSFUL
// activations.
type PinArmStats struct {
	// Activations is the total number of activations attempted in this arm.
	Activations int
	// Succeeded is the number that activated successfully.
	Succeeded int
	// SuccessRate is Succeeded / Activations, or 0 when no activations ran.
	SuccessRate float64
	// Latency is the activate-latency distribution over the successful
	// activations, summarized with the same nearest-rank percentiles as every
	// other latency view in this package.
	Latency Summary
}

// PinningComparison is the pinning-on-vs-off result: the two arms plus the
// headline success-rate lift pinning buys under a claim storm. The honest #168
// deliverable is this comparison measured on the bare-metal node (#16); until
// then every number here is a TARGET, produced only by a real KVM claim storm,
// never invented.
type PinningComparison struct {
	// Off is the arm with pinning disabled (baseline).
	Off PinArmStats
	// On is the arm with pinning enabled (post-ready pin + launch RT priority).
	On PinArmStats
	// SuccessRateLift is On.SuccessRate - Off.SuccessRate: the activate-success
	// improvement pinning buys. Positive when pinning helped.
	SuccessRateLift float64
}

// AggregatePinning derives a PinningComparison from the off and on arms. The
// inputs are not mutated. An empty arm yields zeroed stats rather than a panic,
// so a host that could measure only one side still produces a well-formed
// comparison.
func AggregatePinning(off, on PinningArm) PinningComparison {
	offStats := pinArmStats(off)
	onStats := pinArmStats(on)
	return PinningComparison{
		Off:             offStats,
		On:              onStats,
		SuccessRateLift: onStats.SuccessRate - offStats.SuccessRate,
	}
}

func pinArmStats(arm PinningArm) PinArmStats {
	stats := PinArmStats{Activations: len(arm.Outcomes)}
	var lats []time.Duration
	for _, o := range arm.Outcomes {
		if o.OK {
			stats.Succeeded++
			lats = append(lats, o.Latency)
		}
	}
	if stats.Activations > 0 {
		stats.SuccessRate = float64(stats.Succeeded) / float64(stats.Activations)
	}
	stats.Latency = Summarize(lats)
	return stats
}

// Table renders the comparison as an aligned human-readable block: the per-arm
// success rate and activate-latency distribution, and the headline success-rate
// lift.
func (c PinningComparison) Table() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%-22s %12s %12s\n", "", "pinning-off", "pinning-on")
	fmt.Fprintf(&b, "%-22s %12d %12d\n", "activations", c.Off.Activations, c.On.Activations)
	fmt.Fprintf(&b, "%-22s %12d %12d\n", "succeeded", c.Off.Succeeded, c.On.Succeeded)
	fmt.Fprintf(&b, "%-22s %11.1f%% %11.1f%%\n", "success-rate", c.Off.SuccessRate*100, c.On.SuccessRate*100)
	fmt.Fprintf(&b, "%-22s %12s %12s\n", "activate p50", ms(c.Off.Latency.P50), ms(c.On.Latency.P50))
	fmt.Fprintf(&b, "%-22s %12s %12s\n", "activate p99", ms(c.Off.Latency.P99), ms(c.On.Latency.P99))
	fmt.Fprintf(&b, "%-22s %11.1f%%\n", "success-rate-lift", c.SuccessRateLift*100)
	return b.String()
}

// jsonPinArmStats is the wire view of a PinArmStats. Latency durations are
// exported as integer nanoseconds so the JSON round-trips losslessly.
type jsonPinArmStats struct {
	Activations int         `json:"activations"`
	Succeeded   int         `json:"succeeded"`
	SuccessRate float64     `json:"success_rate"`
	Latency     jsonSummary `json:"latency"`
}

type jsonPinningComparison struct {
	Off             jsonPinArmStats `json:"off"`
	On              jsonPinArmStats `json:"on"`
	SuccessRateLift float64         `json:"success_rate_lift"`
}

func pinArmToJSON(a PinArmStats) jsonPinArmStats {
	return jsonPinArmStats{
		Activations: a.Activations,
		Succeeded:   a.Succeeded,
		SuccessRate: a.SuccessRate,
		Latency:     toJSON(a.Latency),
	}
}

func pinArmFromJSON(j jsonPinArmStats) PinArmStats {
	return PinArmStats{
		Activations: j.Activations,
		Succeeded:   j.Succeeded,
		SuccessRate: j.SuccessRate,
		Latency:     fromJSON(j.Latency),
	}
}

// MarshalJSON encodes the comparison via its nanosecond wire view.
func (c PinningComparison) MarshalJSON() ([]byte, error) {
	return json.Marshal(jsonPinningComparison{
		Off:             pinArmToJSON(c.Off),
		On:              pinArmToJSON(c.On),
		SuccessRateLift: c.SuccessRateLift,
	})
}

// UnmarshalJSON decodes a comparison from its nanosecond wire view.
func (c *PinningComparison) UnmarshalJSON(data []byte) error {
	var j jsonPinningComparison
	if err := json.Unmarshal(data, &j); err != nil {
		return err
	}
	c.Off = pinArmFromJSON(j.Off)
	c.On = pinArmFromJSON(j.On)
	c.SuccessRateLift = j.SuccessRateLift
	return nil
}

// WritePinningJSON writes a comparison as indented JSON to w.
func WritePinningJSON(w io.Writer, c PinningComparison) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(c); err != nil {
		return fmt.Errorf("encode pinning comparison: %w", err)
	}
	return nil
}

// ReadPinningJSON decodes a comparison from JSON bytes.
func ReadPinningJSON(data []byte) (PinningComparison, error) {
	var c PinningComparison
	if err := json.Unmarshal(data, &c); err != nil {
		return PinningComparison{}, fmt.Errorf("decode pinning comparison: %w", err)
	}
	return c, nil
}
