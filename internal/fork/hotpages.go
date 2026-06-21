package fork

import (
	"sort"

	"mitos.run/mitos/internal/cas"
)

// FaultRecord is one page fault observed during a snapshot resume: the byte
// offset, within the guest memory file, of the access that faulted. A real
// trace is produced by the userfaultfd handler (Linux/KVM gated, see
// hotpages_linux.go); the SELECTION below that turns a trace into a shippable
// hot-page set is pure and platform-independent, so it is fully unit-testable on
// any host including darwin.
type FaultRecord struct {
	// Offset is the faulting byte offset within the guest memory file. It is
	// floored to its containing page by the selector, so a sub-page offset is
	// fine.
	Offset int64
}

// HotPageSelection parameterizes how a fault trace is reduced to a hot-page set:
// the page granularity, the manifest file the offsets index into, and an
// optional cap on how many pages to keep.
type HotPageSelection struct {
	// PageSizeBytes is the prefetch unit (2 MiB for hugepage-backed memory). Each
	// fault offset is floored to a multiple of this size.
	PageSizeBytes int64
	// File is the manifest file name the offsets index into (conventionally the
	// memory file, "mem").
	File string
	// Cap, when > 0, limits the result to the Cap hottest pages (those faulted
	// most often). Zero means no cap: every distinct faulted page is kept. The cap
	// bounds the prefetch cost so a pathological trace cannot ask the handler to
	// preload the entire image.
	Cap int
}

// SelectHotPages reduces a resume's fault trace to the hot-page set to ship in
// the snapshot manifest. It is the pure capture seam: the userfaultfd handler
// supplies the trace, this turns it into a deterministic, content-addressable
// descriptor.
//
// The reduction, in order:
//  1. floor each fault offset to its containing page (the prefetch unit is a
//     page, so two faults in one page select that one page);
//  2. count faults per page (the per-page frequency is the hotness signal);
//  3. when Cap > 0, keep only the Cap hottest pages, breaking frequency ties by
//     lowest offset so the result is deterministic;
//  4. emit the kept offsets sorted ascending, so the handler prefetches the
//     memory file sequentially and the descriptor's identity does not depend on
//     capture (fault) order.
//
// The result is a cas.HotPageSet ready to stamp onto a manifest. An empty trace
// yields a set with no offsets, which the manifest omits from its canonical
// encoding, preserving the pre-field snapshot digest (#32).
func SelectHotPages(trace []FaultRecord, sel HotPageSelection) cas.HotPageSet {
	set := cas.HotPageSet{PageSizeBytes: sel.PageSizeBytes, File: sel.File}
	if len(trace) == 0 || sel.PageSizeBytes <= 0 {
		return set
	}

	// Count faults per page-aligned offset.
	counts := make(map[int64]int, len(trace))
	for _, f := range trace {
		page := (f.Offset / sel.PageSizeBytes) * sel.PageSizeBytes
		counts[page]++
	}

	type pageCount struct {
		offset int64
		count  int
	}
	pages := make([]pageCount, 0, len(counts))
	for off, c := range counts {
		pages = append(pages, pageCount{offset: off, count: c})
	}

	// When capping, rank by frequency (hottest first), ties broken by lowest
	// offset for determinism, then keep the top Cap.
	if sel.Cap > 0 && len(pages) > sel.Cap {
		sort.Slice(pages, func(i, j int) bool {
			if pages[i].count != pages[j].count {
				return pages[i].count > pages[j].count
			}
			return pages[i].offset < pages[j].offset
		})
		pages = pages[:sel.Cap]
	}

	offsets := make([]int64, 0, len(pages))
	for _, p := range pages {
		offsets = append(offsets, p.offset)
	}
	// Final emission order is ascending offset for sequential prefetch.
	sort.Slice(offsets, func(i, j int) bool { return offsets[i] < offsets[j] })
	set.Offsets = offsets
	return set
}
