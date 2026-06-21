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
	PageSizeKiB      uint64 `json:"page_size_kib"`
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
