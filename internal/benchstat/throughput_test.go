package benchstat

import (
	"bytes"
	"testing"
	"time"
)

func TestAggregateThroughputEmpty(t *testing.T) {
	got := AggregateThroughput(nil)
	if got.Completed != 0 {
		t.Errorf("Completed = %d, want 0", got.Completed)
	}
	if got.AchievedPerSec != 0 {
		t.Errorf("AchievedPerSec = %v, want 0", got.AchievedPerSec)
	}
	if got.PerNodeDensity != nil {
		t.Errorf("PerNodeDensity = %v, want nil", got.PerNodeDensity)
	}
}

func TestAggregateThroughputSingle(t *testing.T) {
	// One completion has no measurable window, so no rate.
	got := AggregateThroughput([]Completion{{Offset: 100 * time.Millisecond, Concurrent: 1, Node: "n1"}})
	if got.Completed != 1 {
		t.Errorf("Completed = %d, want 1", got.Completed)
	}
	if got.Window != 0 {
		t.Errorf("Window = %v, want 0", got.Window)
	}
	if got.AchievedPerSec != 0 {
		t.Errorf("AchievedPerSec = %v, want 0 (single point has no rate)", got.AchievedPerSec)
	}
	if got.PeakConcurrent != 1 {
		t.Errorf("PeakConcurrent = %d, want 1", got.PeakConcurrent)
	}
	if got.PerNodeDensity["n1"] != 1 {
		t.Errorf("PerNodeDensity[n1] = %d, want 1", got.PerNodeDensity["n1"])
	}
}

func TestAggregateThroughputRate(t *testing.T) {
	// 10 completions spread across exactly 2s (offsets 0s..2s in 9 even steps).
	// Window is 2s, so achieved rate is 10/2 = 5/sec.
	comps := make([]Completion, 10)
	for i := range comps {
		comps[i] = Completion{
			Offset:     time.Duration(i) * 200 * time.Millisecond, // 0, .2, ... 1.8s
			Concurrent: i + 1,
			Node:       "node-a",
		}
	}
	// Push the last completion to exactly 2s so the window is a clean 2s.
	comps[9].Offset = 2 * time.Second
	got := AggregateThroughput(comps)
	if got.Completed != 10 {
		t.Errorf("Completed = %d, want 10", got.Completed)
	}
	if got.Window != 2*time.Second {
		t.Errorf("Window = %v, want 2s", got.Window)
	}
	if got.AchievedPerSec != 5.0 {
		t.Errorf("AchievedPerSec = %v, want 5.0", got.AchievedPerSec)
	}
	if got.PeakConcurrent != 10 {
		t.Errorf("PeakConcurrent = %d, want 10", got.PeakConcurrent)
	}
	if got.PerNodeDensity["node-a"] != 10 {
		t.Errorf("PerNodeDensity[node-a] = %d, want 10", got.PerNodeDensity["node-a"])
	}
}

func TestAggregateThroughputPerNodeDensity(t *testing.T) {
	comps := []Completion{
		{Offset: 0, Concurrent: 1, Node: "a"},
		{Offset: 1 * time.Second, Concurrent: 2, Node: "b"},
		{Offset: 2 * time.Second, Concurrent: 3, Node: "a"},
		{Offset: 3 * time.Second, Concurrent: 2, Node: "a"},
	}
	got := AggregateThroughput(comps)
	if got.PerNodeDensity["a"] != 3 {
		t.Errorf("PerNodeDensity[a] = %d, want 3", got.PerNodeDensity["a"])
	}
	if got.PerNodeDensity["b"] != 1 {
		t.Errorf("PerNodeDensity[b] = %d, want 1", got.PerNodeDensity["b"])
	}
}

func TestAggregateThroughputDoesNotMutate(t *testing.T) {
	comps := []Completion{
		{Offset: 3 * time.Second, Concurrent: 1, Node: "a"},
		{Offset: 1 * time.Second, Concurrent: 2, Node: "a"},
	}
	_ = AggregateThroughput(comps)
	if comps[0].Offset != 3*time.Second || comps[1].Offset != 1*time.Second {
		t.Errorf("input was reordered: %v", comps)
	}
}

func TestThroughputTableDeterministic(t *testing.T) {
	comps := []Completion{
		{Offset: 0, Concurrent: 1, Node: "zeta"},
		{Offset: 1 * time.Second, Concurrent: 2, Node: "alpha"},
	}
	got := AggregateThroughput(comps).Table()
	for _, want := range []string{"completed", "window", "achieved/sec", "peak concurrent", "alpha", "zeta"} {
		if !bytes.Contains([]byte(got), []byte(want)) {
			t.Errorf("Table() missing %q:\n%s", want, got)
		}
	}
	// alpha must sort before zeta in the per-node rows.
	if bytes.Index([]byte(got), []byte("alpha")) > bytes.Index([]byte(got), []byte("zeta")) {
		t.Errorf("per-node rows not sorted:\n%s", got)
	}
}
