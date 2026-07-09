package fork

// This file holds the platform-independent core of the Firecracker userfaultfd
// (UFFD) memory backend used to restore snapshots (issue #167). The syscall-level
// handler is Linux-only (uffd_linux.go) with a non-Linux stub (uffd_other.go);
// the region/offset arithmetic below is pure and unit-tested on any host.
//
// Why a UFFD backend at all: Firecracker refuses to restore a hugetlbfs-backed
// snapshot through the plain file-mapping backend ("Cannot restore hugetlbfs
// backed snapshot by mapping the memory file. Please use uffd."). With the UFFD
// backend Firecracker creates a userfaultfd over the guest memory, hands it to an
// external handler over a unix socket, and the handler services page faults by
// copying page contents out of the snapshot mem file. That same handler can
// PRELOAD a captured hot-page working set before the VM resumes, paying the
// lazy-fault tail up front (the prefetch win), and can RECORD the faults it
// serves to capture that working set in the first place.

// uffdMapping is one guest memory region Firecracker sends the handler over the
// backend socket on restore. The JSON field names match Firecracker's
// GuestRegionUffdMapping. BaseHostVirtAddr is where the region is mapped in
// Firecracker's address space (UFFDIO_COPY destinations are addresses in it);
// Offset is the region's offset within the snapshot mem file (the source of page
// contents); PageSizeKiB is the backing page size (4 for base pages, 2048 for
// 2 MiB hugepages).
type uffdMapping struct {
	BaseHostVirtAddr uint64 `json:"base_host_virt_addr"`
	Size             uint64 `json:"size"`
	Offset           uint64 `json:"offset"`
	// PageSize is the backing page size in BYTES (Firecracker's "page_size"
	// field: 4096 for base pages, 2097152 for 2 MiB hugepages).
	PageSize uint64 `json:"page_size"`
	// PageSizeKiB is Firecracker's "page_size_kib" field. Despite the name,
	// observed Firecracker v1.15 emits it in BYTES (e.g. 2097152 for a 2 MiB
	// page), identical to page_size; it is parsed only as a fallback for a
	// Firecracker version that omits page_size.
	PageSizeKiB uint64 `json:"page_size_kib"`
}

// pageSizeBytes returns the region's backing page size in bytes, preferring the
// unambiguous page_size field and falling back to page_size_kib (treated as bytes
// as Firecracker emits it). Defaults to 4 KiB when neither is set.
func (m uffdMapping) pageSizeBytes() uint64 {
	if m.PageSize > 0 {
		return m.PageSize
	}
	if m.PageSizeKiB > 0 {
		return m.PageSizeKiB
	}
	return 4096
}

// containsAddr reports whether the region covers host address addr.
func (m uffdMapping) containsAddr(addr uint64) bool {
	return addr >= m.BaseHostVirtAddr && addr < m.BaseHostVirtAddr+m.Size
}

// containsOffset reports whether the region covers mem-file offset off.
func (m uffdMapping) containsOffset(off uint64) bool {
	return off >= m.Offset && off < m.Offset+m.Size
}

// fileOffsetForAddr maps a faulting host address to the page-aligned base address
// of its page and the corresponding offset within the snapshot mem file, using
// the region that covers it. pageSize must be positive (the region's backing page
// size). ok is false when no region covers the address, which the handler treats
// as a fault it cannot service.
func fileOffsetForAddr(regions []uffdMapping, addr uint64, pageSize uint64) (pageBase uint64, fileOffset uint64, ok bool) {
	if pageSize == 0 {
		return 0, 0, false
	}
	pageBase = (addr / pageSize) * pageSize
	for _, r := range regions {
		if r.containsAddr(pageBase) {
			return pageBase, r.Offset + (pageBase - r.BaseHostVirtAddr), true
		}
	}
	return 0, 0, false
}

// hostAddrForFileOffset maps a mem-file offset (as stored in a HotPageSet) to the
// host address the handler must UFFDIO_COPY that page into, using the region that
// covers the offset. ok is false when no region covers it, which the handler
// treats as a hot page outside the restored range and skips.
func hostAddrForFileOffset(regions []uffdMapping, fileOffset uint64) (hostAddr uint64, ok bool) {
	for _, r := range regions {
		if r.containsOffset(fileOffset) {
			return r.BaseHostVirtAddr + (fileOffset - r.Offset), true
		}
	}
	return 0, false
}

// lazyChunkBytes is the granularity the live-cow lazy restore populates guest RAM at.
//
// It trades round trips against write amplification. A MISSING fault costs a
// userspace round trip (~6 us), so serving one 4 KiB page per fault would take ~131k
// round trips to fill a 512 MiB guest. But a restoring guest touches pages
// SCATTERED across its address space, not sequentially, so a chunk that is too large
// copies the whole chunk to satisfy a single page: at 2 MiB the prod activate spent
// ~88 ms faulting in ~194 MiB for a far smaller working set, which merely moved the
// eager copy's cost out of vmstate_restore and into guest_ready + handshake.
//
// Measured on the reference node against the real python template, as warm-claim
// activate P50 (bench/results/2026-07-09-lazy-livecow-restore.md): 2 MiB faulted in
// ~194 MiB at 114.4 ms, 256 KiB ~125 MiB at 89.6 ms, 64 KiB ~72 MiB at 76.2 ms.
// 64 KiB (16 pages) trims amplification while keeping the per-fault copy (~14 us)
// close to the round-trip cost.
const lazyChunkBytes = 64 << 10

// lazyChunkForAddr expands a faulting host address into the chunk the handler should
// populate: the lazyChunkBytes-aligned span (in the CONTAINING region's coordinates)
// that covers addr, clipped to the region end so a chunk never straddles two regions
// or reads past the mem file's mapping of that region.
//
// It returns the host address to UFFDIO_COPY into, the mem-file offset to read from,
// and the length. ok is false when addr lies outside every registered region.
func lazyChunkForAddr(regions []uffdMapping, addr uint64) (dst uint64, fileOffset uint64, length uint64, ok bool) {
	for _, r := range regions {
		if r.Size == 0 || addr < r.BaseHostVirtAddr || addr >= r.BaseHostVirtAddr+r.Size {
			continue
		}
		inRegion := addr - r.BaseHostVirtAddr
		start := (inRegion / lazyChunkBytes) * lazyChunkBytes
		length = lazyChunkBytes
		if start+length > r.Size {
			length = r.Size - start
		}
		return r.BaseHostVirtAddr + start, r.Offset + start, length, true
	}
	return 0, 0, 0, false
}
