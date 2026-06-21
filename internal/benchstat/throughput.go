package benchstat

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Throughput is the achieved-rate view of a sustained-load run: a set of
// completed units (claims) over a wall-clock window, plus the peak concurrent
// density observed during the window. It is the aggregation behind the
// "sustained claims/sec" and "density curve" harness (issue #15): the harness
// records each completion's wall-clock offset and the concurrency at that
// instant, and AggregateThroughput turns those raw observations into the
// published rate and density numbers.
//
// It carries no measurement of its own: every field is derived from samples the
// caller collected against a live cluster, so the number stays reproducible per
// the no-unverified-claims rule (CLAUDE.md operating principle 1).
type Throughput struct {
	// Completed is the number of units (claims) that reached the target state
	// inside the window.
	Completed int
	// Window is the wall-clock span from the first completion to the last.
	Window time.Duration
	// AchievedPerSec is Completed divided by Window in seconds. Zero when fewer
	// than two completions were recorded (no measurable window).
	AchievedPerSec float64
	// PeakConcurrent is the maximum number of units observed concurrently active
	// during the window: the density datapoint.
	PeakConcurrent int
	// PerNodeDensity maps node name to the peak number of units placed on that
	// node, the per-node density datapoint. Nil when no per-node data was given.
	PerNodeDensity map[string]int
}

// Completion is one recorded unit completion in a sustained-load run: the
// wall-clock offset from the run start at which the unit reached the target
// state, the number of units concurrently active at that instant, and the node
// the unit was placed on (empty if unknown).
type Completion struct {
	// Offset is the wall-clock time from the run start to this completion.
	Offset time.Duration
	// Concurrent is the number of units active at this completion instant.
	Concurrent int
	// Node is the node the unit landed on, or "" if not recorded.
	Node string
}

// AggregateThroughput derives a Throughput from per-unit completions. The input
// is not mutated. With zero or one completion the window is zero and
// AchievedPerSec is zero (a single point has no measurable rate), but Completed,
// PeakConcurrent, and PerNodeDensity are still reported.
//
// PerNodeDensity counts the peak units seen per node across the completions:
// each completion contributes one unit to its node's running count. This is the
// density-at-node datapoint, not a rate.
func AggregateThroughput(completions []Completion) Throughput {
	t := Throughput{Completed: len(completions)}
	if len(completions) == 0 {
		return t
	}

	offsets := make([]time.Duration, len(completions))
	for i, c := range completions {
		offsets[i] = c.Offset
		if c.Concurrent > t.PeakConcurrent {
			t.PeakConcurrent = c.Concurrent
		}
	}
	sort.Slice(offsets, func(i, j int) bool { return offsets[i] < offsets[j] })
	t.Window = offsets[len(offsets)-1] - offsets[0]
	if t.Window > 0 {
		t.AchievedPerSec = float64(t.Completed) / t.Window.Seconds()
	}

	density := make(map[string]int)
	for _, c := range completions {
		if c.Node != "" {
			density[c.Node]++
		}
	}
	if len(density) > 0 {
		t.PerNodeDensity = density
	}
	return t
}

// Table renders the Throughput as an aligned human-readable block. Per-node
// density rows are sorted by node name so the output is deterministic.
func (t Throughput) Table() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%-18s %10d\n", "completed", t.Completed)
	fmt.Fprintf(&b, "%-18s %10s\n", "window", fmt.Sprintf("%.3f s", t.Window.Seconds()))
	fmt.Fprintf(&b, "%-18s %10s\n", "achieved/sec", fmt.Sprintf("%.3f", t.AchievedPerSec))
	fmt.Fprintf(&b, "%-18s %10d\n", "peak concurrent", t.PeakConcurrent)
	if len(t.PerNodeDensity) > 0 {
		nodes := make([]string, 0, len(t.PerNodeDensity))
		for n := range t.PerNodeDensity {
			nodes = append(nodes, n)
		}
		sort.Strings(nodes)
		for _, n := range nodes {
			fmt.Fprintf(&b, "  node %-12s %10d\n", n, t.PerNodeDensity[n])
		}
	}
	return b.String()
}
