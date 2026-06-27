package casgc

import (
	"errors"
	"testing"
)

func TestTarget(t *testing.T) {
	cases := []struct {
		name             string
		used, total, cas int64
		high, low        float64
		wantMax          int64
		wantEvict        bool
	}{
		{name: "below high watermark: no evict", used: 50, total: 100, cas: 30, high: 0.85, low: 0.70, wantEvict: false},
		{name: "above high: evict cas down to bring fs to low", used: 90, total: 100, cas: 30, high: 0.85, low: 0.70, wantMax: 10, wantEvict: true}, // free 20 -> cas 30-20=10
		{name: "need to free more than cas holds: evict all", used: 95, total: 100, cas: 10, high: 0.85, low: 0.70, wantMax: 0, wantEvict: true},    // free 25 > cas 10 -> 0
		{name: "empty cas: nothing to do", used: 95, total: 100, cas: 0, high: 0.85, low: 0.70, wantEvict: false},
		{name: "zero total: nothing to do", used: 0, total: 0, cas: 10, high: 0.85, low: 0.70, wantEvict: false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotMax, gotEvict := Target(c.used, c.total, c.cas, c.high, c.low)
			if gotEvict != c.wantEvict {
				t.Fatalf("evict = %v, want %v", gotEvict, c.wantEvict)
			}
			if gotEvict && gotMax != c.wantMax {
				t.Fatalf("maxBytes = %d, want %d", gotMax, c.wantMax)
			}
		})
	}
}

type fakeStore struct {
	cas        int64
	evictedTo  int64
	evictCalls int
}

func (f *fakeStore) TotalBytes() (int64, error) { return f.cas, nil }
func (f *fakeStore) EvictToFit(maxBytes int64) (int64, error) {
	f.evictCalls++
	f.evictedTo = maxBytes
	freed := f.cas - maxBytes
	if freed < 0 {
		freed = 0
	}
	return freed, nil
}

func TestTickEvictsOnlyAboveWatermark(t *testing.T) {
	logf := func(string, ...any) {}

	// Below high watermark: no eviction.
	below := &fakeStore{cas: 30}
	tick(below, "/data", 0.85, 0.70, func(string) (int64, int64, error) { return 50, 100, nil }, logf)
	if below.evictCalls != 0 {
		t.Fatalf("evicted below the watermark (%d calls)", below.evictCalls)
	}

	// Above high watermark: evict down to the computed target (free 20 of cas 30 -> 10).
	above := &fakeStore{cas: 30}
	tick(above, "/data", 0.85, 0.70, func(string) (int64, int64, error) { return 90, 100, nil }, logf)
	if above.evictCalls != 1 || above.evictedTo != 10 {
		t.Fatalf("evict calls=%d to=%d, want 1 to=10", above.evictCalls, above.evictedTo)
	}
}

func TestTickToleratesDiskError(t *testing.T) {
	s := &fakeStore{cas: 100}
	tick(s, "/data", 0.85, 0.70, func(string) (int64, int64, error) { return 0, 0, errors.New("statfs boom") }, func(string, ...any) {})
	if s.evictCalls != 0 {
		t.Fatal("evicted despite a disk-usage error")
	}
}
