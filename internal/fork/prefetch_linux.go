//go:build linux

package fork

import (
	"fmt"
	"os"

	"mitos.run/mitos/internal/cas"
)

// This file is the Linux/KVM-gated userfaultfd skeleton for snapshot-resume
// page-fault prefetch (issue #167). It compiles under GOOS=linux and defines the
// real shape of the prefetch path, but the syscall-level userfaultfd register /
// ioctl / page-copy wiring is left for the bare-metal work (it needs a live KVM
// host and a hugepage-backed memory file to exercise). The functions are
// structured so that filling them in is a localized, well-scoped change: the
// surrounding capture-selection (SelectHotPages) and manifest plumbing are
// already done and unit-tested.
//
// Design (see docs/perf/snapshot-prefetch.md):
//   - The guest memory file is backed by 2 MiB hugepages (Firecracker hugetlbfs)
//     so each fault moves a 2 MiB page, not a 4 KiB one, cutting the fault COUNT.
//   - On restore the memory file is mapped with userfaultfd registered over its
//     range. A handler goroutine services UFFD events.
//   - Before the VM is resumed, the handler PRELOADS the manifest's captured
//     hot-page set (cas.HotPageSet) by faulting those pages in, so the post-resume
//     fault storm is paid up front and claim->first-exec drops.
//   - During a CAPTURE resume (template build or first warm), the handler records
//     each serviced fault as a FaultRecord; SelectHotPages reduces the trace to
//     the set stamped onto the manifest.

// PrefetchHandler owns the userfaultfd file descriptor registered over a
// restored VM's guest memory mapping. Its zero value is not usable; construct it
// with newPrefetchHandler.
//
// Lifecycle: register (newPrefetchHandler) -> optional Preload(hot set) before
// resume -> Serve in a goroutine for the life of the VM -> Close on teardown. In
// capture mode Serve also appends every serviced fault to the trace, which
// CaptureTrace returns.
type PrefetchHandler struct {
	// uffd is the userfaultfd file descriptor. The real implementation obtains it
	// via the userfaultfd(2) syscall and UFFDIO_REGISTER over the guest memory
	// range; until that wiring lands it is the zero file.
	uffd *os.File
	// memPath is the guest memory file the fd is registered over.
	memPath string
	// pageSize is the prefetch unit in bytes (2 MiB for hugepage-backed memory).
	pageSize int64
	// capture, when true, makes Serve record every serviced fault into trace.
	capture bool
	// trace accumulates serviced faults during a capture resume.
	trace []FaultRecord
}

// newPrefetchHandler registers userfaultfd over the guest memory file at memPath
// and returns a handler ready to Preload and Serve.
//
// NOT YET WIRED: the syscall sequence (userfaultfd(2), UFFDIO_API,
// UFFDIO_REGISTER over the mmap'd memory range) needs a live KVM host with a
// hugepage-backed memory file to exercise, so it is deferred to the bare-metal
// work. The signature and ownership are fixed so the caller (the restore path)
// can be wired against this shape now.
func newPrefetchHandler(memPath string, pageSize int64, capture bool) (*PrefetchHandler, error) {
	if pageSize <= 0 {
		return nil, fmt.Errorf("prefetch: page size must be positive, got %d", pageSize)
	}
	// The handler struct is fully populated so the caller (the restore path) can
	// be wired against the real shape now. The userfaultfd fd itself is obtained
	// by register, which is the deferred syscall work; until then uffd is nil and
	// the operations below report not-yet-wired.
	return &PrefetchHandler{memPath: memPath, pageSize: pageSize, capture: capture}, nil
}

// register obtains the userfaultfd file descriptor and registers it over the
// guest memory mapping. NOT YET WIRED: this is the deferred syscall sequence
// (userfaultfd(2), UFFDIO_API, UFFDIO_REGISTER over the mmap'd range). Preload
// and Serve call it before they touch the fd, so registration is lazy and the
// caller can hold a configured handler before the fd exists.
func (h *PrefetchHandler) register() error {
	return fmt.Errorf("prefetch: userfaultfd register not yet wired on this build; track issue #167 (memPath=%s pageSize=%d capture=%t)", h.memPath, h.pageSize, h.capture)
}

// Preload faults in the captured hot-page set before the VM is resumed, paying
// the lazy-fault tail up front. set.File must name the memory file this handler
// is registered over; offsets outside the mapped range are skipped.
//
// NOT YET WIRED: the per-page UFFDIO_COPY / UFFDIO_CONTINUE that materializes a
// page is the bare-metal work. The loop shape (iterate the sorted offsets, copy
// each page in) is fixed here so the captured set is consumed in exactly the
// order SelectHotPages emits it (ascending, sequential).
func (h *PrefetchHandler) Preload(set cas.HotPageSet) error {
	if h == nil {
		return fmt.Errorf("prefetch: nil handler")
	}
	if err := h.register(); err != nil {
		return err
	}
	_ = set
	return fmt.Errorf("prefetch: Preload not yet wired on this build; track issue #167")
}

// Serve runs the userfaultfd event loop until Close, copying in each faulting
// page on demand (the pages NOT preloaded). In capture mode it also appends each
// serviced fault to the trace.
//
// NOT YET WIRED: the poll/read on the uffd fd and the UFFDIO_COPY response are
// the bare-metal work.
func (h *PrefetchHandler) Serve() error {
	if h == nil {
		return fmt.Errorf("prefetch: nil handler")
	}
	if err := h.register(); err != nil {
		return err
	}
	return fmt.Errorf("prefetch: Serve not yet wired on this build; track issue #167")
}

// CaptureTrace returns the faults serviced so far in a capture resume, in the
// order they were serviced. The caller hands this to SelectHotPages to produce
// the manifest's hot-page set. It is meaningful only when the handler was
// constructed with capture=true.
func (h *PrefetchHandler) CaptureTrace() []FaultRecord {
	if h == nil {
		return nil
	}
	return h.trace
}

// Close tears down the userfaultfd registration and fd.
func (h *PrefetchHandler) Close() error {
	if h == nil || h.uffd == nil {
		return nil
	}
	return h.uffd.Close()
}
