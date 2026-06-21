package benchstat

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
)

// PrefetchArm is the raw observations from one side of a prefetch-on-vs-off run
// (issue #167): per-resume fault counts and per-resume claim->first-exec
// latencies. It carries no aggregation of its own; AggregatePrefetch turns two
// arms into a PrefetchComparison. The samples are collected against the real
// KVM-backed engine (the userfaultfd handler reports the fault count per resume);
// off-KVM the bench driver never produces them, so no number is fabricated.
type PrefetchArm struct {
	// FaultCounts is the number of page faults serviced per resume, one entry per
	// measured resume.
	FaultCounts []int
	// ClaimToExec is the claim->first-exec latency per resume, one entry per
	// measured resume.
	ClaimToExec []time.Duration
}

// ArmStats is the aggregated view of one arm: the mean fault count per resume
// and the claim->first-exec latency distribution.
type ArmStats struct {
	// Resumes is the number of resumes measured in this arm.
	Resumes int
	// MeanFaults is the mean page-fault count per resume. Zero when no resumes
	// were measured.
	MeanFaults int
	// ClaimToExec is the claim->first-exec latency distribution for the arm,
	// summarized with the same nearest-rank percentiles as every other latency
	// view in this package.
	ClaimToExec Summary
}

// PrefetchComparison is the prefetch-on-vs-off result: the two arms plus the
// headline fault-count reduction prefetch buys. The honest #167 deliverable is
// this comparison measured on the bare-metal node (#16); until then every number
// here is a TARGET, produced only by a real KVM run, never invented.
type PrefetchComparison struct {
	// Off is the arm with prefetch disabled (lazy-fault baseline).
	Off ArmStats
	// On is the arm with prefetch enabled (hot-page set preloaded before resume).
	On ArmStats
	// FaultReduction is Off.MeanFaults - On.MeanFaults: the mean faults per resume
	// prefetch eliminates. Positive when prefetch helped.
	FaultReduction int
}

// AggregatePrefetch derives a PrefetchComparison from the off and on arms. The
// inputs are not mutated. An empty arm yields zeroed stats rather than a panic,
// so a host that could measure only one side still produces a well-formed
// comparison.
func AggregatePrefetch(off, on PrefetchArm) PrefetchComparison {
	offStats := armStats(off)
	onStats := armStats(on)
	return PrefetchComparison{
		Off:            offStats,
		On:             onStats,
		FaultReduction: offStats.MeanFaults - onStats.MeanFaults,
	}
}

func armStats(arm PrefetchArm) ArmStats {
	stats := ArmStats{
		Resumes:     len(arm.FaultCounts),
		ClaimToExec: Summarize(arm.ClaimToExec),
	}
	if len(arm.FaultCounts) > 0 {
		var total int
		for _, c := range arm.FaultCounts {
			total += c
		}
		stats.MeanFaults = total / len(arm.FaultCounts)
	}
	return stats
}

// Table renders the comparison as an aligned human-readable block: the per-arm
// mean fault count and claim->first-exec distribution, and the headline fault
// reduction.
func (c PrefetchComparison) Table() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%-22s %12s %12s\n", "", "prefetch-off", "prefetch-on")
	fmt.Fprintf(&b, "%-22s %12d %12d\n", "resumes", c.Off.Resumes, c.On.Resumes)
	fmt.Fprintf(&b, "%-22s %12d %12d\n", "mean-faults", c.Off.MeanFaults, c.On.MeanFaults)
	fmt.Fprintf(&b, "%-22s %12s %12s\n", "claim->exec p50", ms(c.Off.ClaimToExec.P50), ms(c.On.ClaimToExec.P50))
	fmt.Fprintf(&b, "%-22s %12s %12s\n", "claim->exec p99", ms(c.Off.ClaimToExec.P99), ms(c.On.ClaimToExec.P99))
	fmt.Fprintf(&b, "%-22s %12d\n", "fault-reduction", c.FaultReduction)
	return b.String()
}

// jsonArmStats is the wire view of an ArmStats. Latency durations are exported
// as integer nanoseconds so the JSON round-trips losslessly.
type jsonArmStats struct {
	Resumes     int         `json:"resumes"`
	MeanFaults  int         `json:"mean_faults"`
	ClaimToExec jsonSummary `json:"claim_to_exec"`
}

type jsonPrefetchComparison struct {
	Off            jsonArmStats `json:"off"`
	On             jsonArmStats `json:"on"`
	FaultReduction int          `json:"fault_reduction"`
}

func armToJSON(a ArmStats) jsonArmStats {
	return jsonArmStats{Resumes: a.Resumes, MeanFaults: a.MeanFaults, ClaimToExec: toJSON(a.ClaimToExec)}
}

func armFromJSON(j jsonArmStats) ArmStats {
	return ArmStats{Resumes: j.Resumes, MeanFaults: j.MeanFaults, ClaimToExec: fromJSON(j.ClaimToExec)}
}

// MarshalJSON encodes the comparison via its nanosecond wire view.
func (c PrefetchComparison) MarshalJSON() ([]byte, error) {
	return json.Marshal(jsonPrefetchComparison{
		Off:            armToJSON(c.Off),
		On:             armToJSON(c.On),
		FaultReduction: c.FaultReduction,
	})
}

// UnmarshalJSON decodes a comparison from its nanosecond wire view.
func (c *PrefetchComparison) UnmarshalJSON(data []byte) error {
	var j jsonPrefetchComparison
	if err := json.Unmarshal(data, &j); err != nil {
		return err
	}
	c.Off = armFromJSON(j.Off)
	c.On = armFromJSON(j.On)
	c.FaultReduction = j.FaultReduction
	return nil
}

// WritePrefetchJSON writes a comparison as indented JSON to w.
func WritePrefetchJSON(w io.Writer, c PrefetchComparison) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(c); err != nil {
		return fmt.Errorf("encode prefetch comparison: %w", err)
	}
	return nil
}

// ReadPrefetchJSON decodes a comparison from JSON bytes.
func ReadPrefetchJSON(data []byte) (PrefetchComparison, error) {
	var c PrefetchComparison
	if err := json.Unmarshal(data, &c); err != nil {
		return PrefetchComparison{}, fmt.Errorf("decode prefetch comparison: %w", err)
	}
	return c, nil
}
