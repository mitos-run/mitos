package benchstat

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
)

// ChildReady is one recorded child outcome in a 1-to-N fan-out run: forking ONE
// warmed base (repo loaded, deps installed) into N children. TimeToReady is the
// child's own fork->ready span (the per-child latency sample); ReadyOffset is
// the wall-clock instant, measured from the fan-out start, at which the child
// became ready. The two differ when forks are launched in waves or staggered:
// TimeToReady isolates one child's cost, ReadyOffset places it on the shared
// wall clock so the slowest child defines when all N are ready.
type ChildReady struct {
	// TimeToReady is this child's own fork->ready duration.
	TimeToReady time.Duration
	// ReadyOffset is the wall-clock offset from the fan-out start to the instant
	// this child became ready.
	ReadyOffset time.Duration
}

// FanOut is the aggregation behind the 1-to-N live-fork fan-out shape (issue
// #207): forking ONE warmed base into N children, then reporting (a) the
// per-child time-to-ready distribution and (b) the wall-clock to all N ready.
//
// It carries no measurement of its own: every field is derived from samples the
// caller collected against the real engine, so the number stays reproducible per
// the no-unverified-claims rule (CLAUDE.md operating principle 1).
type FanOut struct {
	// Children is the number of children forked from the one base (N).
	Children int
	// WallClockToReady is the wall clock from the fan-out start to the instant
	// the LAST child became ready: N children are all ready only when the
	// slowest finishes. It is the max ReadyOffset across all children, zero when
	// no children were recorded.
	WallClockToReady time.Duration
	// PerChild is the distribution of per-child time-to-ready samples (the
	// TimeToReady values), summarized with the same nearest-rank percentiles as
	// every other latency view in this package.
	PerChild Summary
	// RawTimeToReady is the raw per-child time-to-ready samples in input order:
	// the ground truth behind PerChild, preserved so downstream tooling (for
	// example the competitor fan-out comparison harness) can re-derive the
	// distribution by its own method rather than trusting a pre-aggregated
	// number. Nil when no children were recorded.
	RawTimeToReady []time.Duration
}

// AggregateFanOut derives a FanOut from per-child outcomes. The input is not
// mutated. With zero children the result is the zero FanOut. The per-child
// distribution is over TimeToReady; the wall-clock-to-N-ready is the maximum
// ReadyOffset, since N children are ready only once the slowest is ready.
func AggregateFanOut(children []ChildReady) FanOut {
	fo := FanOut{Children: len(children)}
	if len(children) == 0 {
		return fo
	}

	samples := make([]time.Duration, len(children))
	var maxOffset time.Duration
	for i, c := range children {
		samples[i] = c.TimeToReady
		if c.ReadyOffset > maxOffset {
			maxOffset = c.ReadyOffset
		}
	}
	fo.WallClockToReady = maxOffset
	fo.PerChild = Summarize(samples)
	fo.RawTimeToReady = samples
	return fo
}

// Table renders the FanOut as an aligned human-readable block: the headline
// counts, the wall-clock-to-N-ready, and the per-child time-to-ready
// distribution.
func (f FanOut) Table() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%-22s %10d\n", "children", f.Children)
	fmt.Fprintf(&b, "%-22s %10s\n", "wall-clock-to-ready", ms(f.WallClockToReady))
	b.WriteString("per-child time-to-ready:\n")
	b.WriteString(f.PerChild.Table())
	return b.String()
}

// FanOutResult names a FanOut for a particular N (one point on the fan-out
// curve over N = 1, 4, 16, 64, ...).
type FanOutResult struct {
	// N is the fan-out width this result was measured at.
	N int
	// Name labels the measurement (for example "fork_fanout").
	Name string
	// FanOut is the aggregated distribution at this N.
	FanOut FanOut
}

// jsonFanOut is the wire view of a FanOut. Durations are exported as integer
// nanoseconds so the JSON round-trips losslessly back into a time.Duration.
type jsonFanOut struct {
	Children           int         `json:"children"`
	WallClockToReadyNs int64       `json:"wall_clock_to_ready_ns"`
	PerChild           jsonSummary `json:"per_child"`
	RawTimeToReadyNs   []int64     `json:"raw_time_to_ready_ns,omitempty"`
}

type jsonFanOutResult struct {
	N      int        `json:"n"`
	Name   string     `json:"name"`
	FanOut jsonFanOut `json:"fanout"`
}

// MarshalJSON encodes the FanOutResult via its nanosecond wire view.
func (r FanOutResult) MarshalJSON() ([]byte, error) {
	var raw []int64
	if len(r.FanOut.RawTimeToReady) > 0 {
		raw = make([]int64, len(r.FanOut.RawTimeToReady))
		for i, d := range r.FanOut.RawTimeToReady {
			raw[i] = d.Nanoseconds()
		}
	}
	return json.Marshal(jsonFanOutResult{
		N:    r.N,
		Name: r.Name,
		FanOut: jsonFanOut{
			Children:           r.FanOut.Children,
			WallClockToReadyNs: r.FanOut.WallClockToReady.Nanoseconds(),
			PerChild:           toJSON(r.FanOut.PerChild),
			RawTimeToReadyNs:   raw,
		},
	})
}

// UnmarshalJSON decodes a FanOutResult from its nanosecond wire view.
func (r *FanOutResult) UnmarshalJSON(data []byte) error {
	var j jsonFanOutResult
	if err := json.Unmarshal(data, &j); err != nil {
		return err
	}
	r.N = j.N
	r.Name = j.Name
	var raw []time.Duration
	if len(j.FanOut.RawTimeToReadyNs) > 0 {
		raw = make([]time.Duration, len(j.FanOut.RawTimeToReadyNs))
		for i, ns := range j.FanOut.RawTimeToReadyNs {
			raw[i] = time.Duration(ns)
		}
	}
	r.FanOut = FanOut{
		Children:         j.FanOut.Children,
		WallClockToReady: time.Duration(j.FanOut.WallClockToReadyNs),
		PerChild:         fromJSON(j.FanOut.PerChild),
		RawTimeToReady:   raw,
	}
	return nil
}

// WriteFanOutJSON writes fan-out results as indented JSON to w.
func WriteFanOutJSON(w io.Writer, results []FanOutResult) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(results); err != nil {
		return fmt.Errorf("encode fan-out results: %w", err)
	}
	return nil
}

// ReadFanOutJSON decodes fan-out results from JSON bytes.
func ReadFanOutJSON(data []byte) ([]FanOutResult, error) {
	var results []FanOutResult
	if err := json.Unmarshal(data, &results); err != nil {
		return nil, fmt.Errorf("decode fan-out results: %w", err)
	}
	return results, nil
}
