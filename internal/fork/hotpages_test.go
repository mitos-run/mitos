package fork

import (
	"testing"

	"mitos.run/mitos/internal/cas"
)

// TestSelectHotPagesDedupes proves the selection collapses repeated faults on
// the same page to a single offset: a real fault trace records a page once per
// fault, and a hot page is faulted many times, so the descriptor must not store
// the same offset more than once.
func TestSelectHotPagesDedupes(t *testing.T) {
	trace := []FaultRecord{
		{Offset: 0}, {Offset: 1 << 21}, {Offset: 0}, {Offset: 1 << 21}, {Offset: 0},
	}
	set := SelectHotPages(trace, HotPageSelection{PageSizeBytes: 2 << 20, File: "mem", Cap: 0})
	if got := len(set.Offsets); got != 2 {
		t.Fatalf("dedupe failed: want 2 offsets, got %d (%v)", got, set.Offsets)
	}
}

// TestSelectHotPagesAlignsToPage proves a raw fault address is floored to its
// containing page before it is recorded: the prefetch unit is a page, so two
// faults inside the same page must select the one page that covers them.
func TestSelectHotPagesAlignsToPage(t *testing.T) {
	const pg = int64(2 << 20)
	trace := []FaultRecord{
		{Offset: pg + 17},     // mid-page
		{Offset: pg + pg - 1}, // end of same page
		{Offset: 2*pg + 5},    // next page
	}
	set := SelectHotPages(trace, HotPageSelection{PageSizeBytes: pg, File: "mem"})
	want := []int64{pg, 2 * pg}
	if len(set.Offsets) != len(want) {
		t.Fatalf("align: want %v, got %v", want, set.Offsets)
	}
	for i := range want {
		if set.Offsets[i] != want[i] {
			t.Fatalf("align offset %d: want %d got %d", i, want[i], set.Offsets[i])
		}
	}
}

// TestSelectHotPagesOrdersByOffset proves the stored offsets are ascending: a
// userfaultfd handler prefetching them touches the memory file sequentially,
// which is the friendliest access pattern for the backing store, so capture
// (fault) order must not leak into the descriptor.
func TestSelectHotPagesOrdersByOffset(t *testing.T) {
	const pg = int64(2 << 20)
	trace := []FaultRecord{{Offset: 4 * pg}, {Offset: 0}, {Offset: 2 * pg}}
	set := SelectHotPages(trace, HotPageSelection{PageSizeBytes: pg, File: "mem"})
	for i := 1; i < len(set.Offsets); i++ {
		if set.Offsets[i] <= set.Offsets[i-1] {
			t.Fatalf("offsets not ascending: %v", set.Offsets)
		}
	}
}

// TestSelectHotPagesCapsByFaultFrequency proves the cap keeps the HOTTEST pages:
// when more distinct pages fault than the cap allows, the pages selected are the
// ones faulted most often, since those dominate the post-resume fault tail and
// give the most latency back per prefetched page.
func TestSelectHotPagesCapsByFaultFrequency(t *testing.T) {
	const pg = int64(2 << 20)
	// page 0 faults 3x, page 1 faults 2x, page 2 faults 1x.
	trace := []FaultRecord{
		{Offset: 0}, {Offset: 0}, {Offset: 0},
		{Offset: pg}, {Offset: pg},
		{Offset: 2 * pg},
	}
	set := SelectHotPages(trace, HotPageSelection{PageSizeBytes: pg, File: "mem", Cap: 2})
	if len(set.Offsets) != 2 {
		t.Fatalf("cap not honored: want 2, got %d (%v)", len(set.Offsets), set.Offsets)
	}
	// The two hottest are pages 0 and 1; the cold page 2 must be dropped.
	for _, off := range set.Offsets {
		if off == 2*pg {
			t.Fatalf("cap dropped a hot page and kept the cold one: %v", set.Offsets)
		}
	}
}

// TestSelectHotPagesEmptyTrace proves an empty trace yields an empty (but
// well-formed) set: a snapshot whose resume never faulted gets a nil offset
// list, which the manifest then omits from its canonical encoding, preserving
// the pre-field digest.
func TestSelectHotPagesEmptyTrace(t *testing.T) {
	set := SelectHotPages(nil, HotPageSelection{PageSizeBytes: 2 << 20, File: "mem"})
	if len(set.Offsets) != 0 {
		t.Fatalf("empty trace produced offsets: %v", set.Offsets)
	}
	if set.PageSizeBytes != 2<<20 || set.File != "mem" {
		t.Fatalf("empty set lost descriptor metadata: %+v", set)
	}
}

// TestSelectHotPagesResultRoundTripsThroughManifest closes the loop: the
// selected set is exactly the cas.HotPageSet that ships in the manifest, so a
// captured set can be stamped onto a snapshot and survive the content-addressed
// round-trip.
func TestSelectHotPagesResultRoundTripsThroughManifest(t *testing.T) {
	const pg = int64(2 << 20)
	trace := []FaultRecord{{Offset: 2 * pg}, {Offset: 0}, {Offset: 0}}
	set := SelectHotPages(trace, HotPageSelection{PageSizeBytes: pg, File: "mem"})

	m := cas.Manifest{
		Files:    []cas.FileEntry{{Name: "mem", Size: 3}},
		HotPages: &set,
	}
	if m.HotPages == nil || len(m.HotPages.Offsets) != 2 {
		t.Fatalf("selected set did not map onto manifest: %+v", m.HotPages)
	}
}
