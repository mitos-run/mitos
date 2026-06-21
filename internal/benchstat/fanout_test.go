package benchstat

import (
	"bytes"
	"testing"
	"time"
)

func TestAggregateFanOutEmpty(t *testing.T) {
	got := AggregateFanOut(nil)
	if got.Children != 0 {
		t.Errorf("Children = %d, want 0", got.Children)
	}
	if got.WallClockToReady != 0 {
		t.Errorf("WallClockToReady = %v, want 0", got.WallClockToReady)
	}
	if got.PerChild.Count != 0 {
		t.Errorf("PerChild.Count = %d, want 0", got.PerChild.Count)
	}
}

func TestAggregateFanOutSingle(t *testing.T) {
	// One child: wall-clock-to-N-ready is that child's ready offset, and the
	// per-child distribution is the single time-to-ready sample.
	got := AggregateFanOut([]ChildReady{
		{TimeToReady: 80 * time.Millisecond, ReadyOffset: 80 * time.Millisecond},
	})
	if got.Children != 1 {
		t.Errorf("Children = %d, want 1", got.Children)
	}
	if got.WallClockToReady != 80*time.Millisecond {
		t.Errorf("WallClockToReady = %v, want 80ms", got.WallClockToReady)
	}
	if got.PerChild.Count != 1 {
		t.Errorf("PerChild.Count = %d, want 1", got.PerChild.Count)
	}
	if got.PerChild.Max != 80*time.Millisecond {
		t.Errorf("PerChild.Max = %v, want 80ms", got.PerChild.Max)
	}
}

func TestAggregateFanOutWallClockIsLastReady(t *testing.T) {
	// Four children forked from one base. Each child's TimeToReady is its own
	// fork->ready span; ReadyOffset is the wall-clock instant (from fan-out
	// start) the child became ready. Wall-clock-to-N-ready is the LATEST
	// ReadyOffset: N children are all ready only when the slowest finishes.
	children := []ChildReady{
		{TimeToReady: 50 * time.Millisecond, ReadyOffset: 50 * time.Millisecond},
		{TimeToReady: 60 * time.Millisecond, ReadyOffset: 110 * time.Millisecond},
		{TimeToReady: 40 * time.Millisecond, ReadyOffset: 150 * time.Millisecond},
		{TimeToReady: 70 * time.Millisecond, ReadyOffset: 220 * time.Millisecond},
	}
	got := AggregateFanOut(children)
	if got.Children != 4 {
		t.Errorf("Children = %d, want 4", got.Children)
	}
	if got.WallClockToReady != 220*time.Millisecond {
		t.Errorf("WallClockToReady = %v, want 220ms (latest ReadyOffset)", got.WallClockToReady)
	}
	// Per-child distribution is over TimeToReady values: min 40, max 70.
	if got.PerChild.Min != 40*time.Millisecond {
		t.Errorf("PerChild.Min = %v, want 40ms", got.PerChild.Min)
	}
	if got.PerChild.Max != 70*time.Millisecond {
		t.Errorf("PerChild.Max = %v, want 70ms", got.PerChild.Max)
	}
}

func TestAggregateFanOutOutOfOrderInput(t *testing.T) {
	// Input order must not matter: wall-clock is the max ReadyOffset regardless.
	children := []ChildReady{
		{TimeToReady: 70 * time.Millisecond, ReadyOffset: 220 * time.Millisecond},
		{TimeToReady: 50 * time.Millisecond, ReadyOffset: 50 * time.Millisecond},
	}
	got := AggregateFanOut(children)
	if got.WallClockToReady != 220*time.Millisecond {
		t.Errorf("WallClockToReady = %v, want 220ms", got.WallClockToReady)
	}
}

func TestAggregateFanOutDoesNotMutate(t *testing.T) {
	children := []ChildReady{
		{TimeToReady: 70 * time.Millisecond, ReadyOffset: 220 * time.Millisecond},
		{TimeToReady: 50 * time.Millisecond, ReadyOffset: 50 * time.Millisecond},
	}
	_ = AggregateFanOut(children)
	if children[0].ReadyOffset != 220*time.Millisecond || children[1].ReadyOffset != 50*time.Millisecond {
		t.Errorf("input was reordered: %v", children)
	}
}

func TestFanOutTableContents(t *testing.T) {
	children := []ChildReady{
		{TimeToReady: 50 * time.Millisecond, ReadyOffset: 50 * time.Millisecond},
		{TimeToReady: 70 * time.Millisecond, ReadyOffset: 220 * time.Millisecond},
	}
	got := AggregateFanOut(children).Table()
	for _, want := range []string{"children", "wall-clock-to-ready", "per-child time-to-ready", "p50", "max"} {
		if !bytes.Contains([]byte(got), []byte(want)) {
			t.Errorf("Table() missing %q:\n%s", want, got)
		}
	}
}

func TestAggregateFanOutRetainsRawSamples(t *testing.T) {
	children := []ChildReady{
		{TimeToReady: 50 * time.Millisecond, ReadyOffset: 50 * time.Millisecond},
		{TimeToReady: 70 * time.Millisecond, ReadyOffset: 220 * time.Millisecond},
		{TimeToReady: 40 * time.Millisecond, ReadyOffset: 260 * time.Millisecond},
	}
	got := AggregateFanOut(children)
	if len(got.RawTimeToReady) != 3 {
		t.Fatalf("RawTimeToReady len = %d, want 3", len(got.RawTimeToReady))
	}
	// Raw samples are in input order (not sorted), so the harness downstream can
	// re-derive any distribution it wants.
	want := []time.Duration{50 * time.Millisecond, 70 * time.Millisecond, 40 * time.Millisecond}
	for i, w := range want {
		if got.RawTimeToReady[i] != w {
			t.Errorf("RawTimeToReady[%d] = %v, want %v", i, got.RawTimeToReady[i], w)
		}
	}
}

func TestFanOutResultJSONRoundTrip(t *testing.T) {
	children := []ChildReady{
		{TimeToReady: 50 * time.Millisecond, ReadyOffset: 50 * time.Millisecond},
		{TimeToReady: 70 * time.Millisecond, ReadyOffset: 220 * time.Millisecond},
	}
	fo := AggregateFanOut(children)
	r := FanOutResult{N: 2, Name: "fork_fanout", FanOut: fo}

	var buf bytes.Buffer
	if err := WriteFanOutJSON(&buf, []FanOutResult{r}); err != nil {
		t.Fatalf("WriteFanOutJSON: %v", err)
	}
	out, err := ReadFanOutJSON(buf.Bytes())
	if err != nil {
		t.Fatalf("ReadFanOutJSON: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("len = %d, want 1", len(out))
	}
	if out[0].N != 2 {
		t.Errorf("N = %d, want 2", out[0].N)
	}
	if out[0].FanOut.WallClockToReady != 220*time.Millisecond {
		t.Errorf("WallClockToReady = %v, want 220ms", out[0].FanOut.WallClockToReady)
	}
	if out[0].FanOut.PerChild.Max != 70*time.Millisecond {
		t.Errorf("PerChild.Max = %v, want 70ms", out[0].FanOut.PerChild.Max)
	}
	if len(out[0].FanOut.RawTimeToReady) != 2 {
		t.Fatalf("RawTimeToReady len = %d, want 2", len(out[0].FanOut.RawTimeToReady))
	}
	if out[0].FanOut.RawTimeToReady[0] != 50*time.Millisecond || out[0].FanOut.RawTimeToReady[1] != 70*time.Millisecond {
		t.Errorf("RawTimeToReady = %v, want [50ms 70ms]", out[0].FanOut.RawTimeToReady)
	}
}
