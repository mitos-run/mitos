package fork

import "errors"

// ErrChildUFFDUnsupported is returned by StartChildUFFDHandler off Linux (the lazy
// child import needs userfaultfd + memfd + /proc fd re-open, all Linux-only). The
// live-cow child-import path fails closed to the disk-snapshot restore in that
// case, exactly like ErrLiveCowUnsupported on the parent side.
var ErrChildUFFDUnsupported = errors.New("fork: lazy live-cow child UFFD import requires Linux userfaultfd")

// This file holds the platform-independent seam of the live copy-on-write
// (live-cow) LAZY child-side memfd import: a co-located fork child boots its guest
// RAM through Firecracker's NATIVE userfaultfd restore backend, faulting only the
// working set it actually touches instead of eagerly copying the whole guest RAM.
//
// Why this exists (the fork-latency fix). The shipped child import
// (ComposeChildFromImport / the FIRECRACKER_MITOS_CHILD_MEMFD restore patch) is a
// vmstate-only fork: it skips the source's ~364ms mem-file write (issue #832). But
// it then EAGERLY copies the whole guest RAM (e.g. 256MiB) from the parent's live
// memfd into the child's private anonymous RAM at restore, so the child's
// vmstate_restore grows from ~20ms to ~391ms and the fork is latency-NEUTRAL: the
// cost just moved from the source snapshot to the child restore.
//
// The eager copy exists to avoid a leak: a lazily file-backed MAP_PRIVATE of the
// RESUMED source's live memfd reads back any page the source rewrites after the
// fork but before the child faults it (a torn kernel image). The LAZY UFFD import
// closes that hole differently: the child restores with anonymous RAM registered
// to a userfaultfd (Firecracker's stock Uffd backend), and a husk-side handler
// fills each faulting page ON DEMAND by composing the SAME per-page source
// selection ComposeChildFromImport does, but PER FAULT: a page the parent's WP
// handler has FROZEN (bit set at fault time) is served from the FROZEN memfd at
// its fork-time value; every other page is served from the LIVE source memfd,
// which still holds the fork-time value because the WP handler freezes a page
// BEFORE it lets the source's write land. Checking the frozen bit AT FAULT time
// (with a re-check after reading the live page) means a source post-fork overwrite
// is always served as its fork-time value, never the mutated value. The child pays
// only the faults for its working set (a few MiB), so the child restore drops back
// to milliseconds and the vmstate-only fork's source win is finally realized end
// to end.
//
// This uses Firecracker's NATIVE Uffd restore backend (MemBackendType::Uffd), so
// NO Firecracker patch is needed on the child side: the husk points
// /snapshot/load at the handler's unix socket via mem_backend.backend_path, and
// Firecracker creates the guest userfaultfd and hands it to the handler over that
// socket (the same GuestRegionUffdMapping SCM_RIGHTS handshake the issue #167 UFFD
// backend already uses). The SOURCE side is unchanged (still the m1 memfd export +
// m2 write-protect offer).
//
// SECURITY (internal/fork is a named-reviewer path). The handler reads ONLY the
// parent's own exported guest memfd and this pod's private FROZEN + bitmap memfds,
// each identity-verified on reopen (openProcFdVerified, PID-reuse fail-closed), and
// writes ONLY into the guest RAM Firecracker registered, via UFFDIO_COPY. It makes
// no host-path write and copies no secret out of the guest. The whole path is dark
// unless --live-cow-fork armed the parent AND a co-located fork child spawn opted
// in; off, the child restores from the disk snapshot byte-for-byte. See
// docs/threat-model.md and docs/fork-correctness.md.

// ChildUFFDHandle is the husk-side lazy UFFD fault handler for one co-located
// live-cow fork child. It is created bound to its unix socket
// (StartChildUFFDHandler) BEFORE the child Firecracker's /snapshot/load, which is
// what drives Firecracker to connect; Receive completes the GuestRegionUffdMapping
// handshake (the child's userfaultfd + region layout) once Firecracker connects;
// Serve fills each faulting guest page from the composed source (FROZEN memfd for a
// frozen page, the live source memfd otherwise) for the life of the child; Close
// tears the handler down. The Linux implementation is childUFFDHandler
// (childuffd_linux.go); off Linux StartChildUFFDHandler returns
// ErrChildUFFDUnsupported and nothing here runs (the child falls back to the disk
// restore, fail-closed).
type ChildUFFDHandle interface {
	// Receive accepts the child Firecracker's connection and reads the child
	// userfaultfd + region layout (the GuestRegionUffdMapping SCM_RIGHTS handshake).
	// Run it concurrently with the child /snapshot/load.
	Receive() error
	// Serve runs the page-fault loop until Close: each faulting guest page is
	// UFFDIO_COPYed in from the composed source (FROZEN at its fork-time value for a
	// frozen page, the live source memfd otherwise), checking the frozen bit AT
	// FAULT time so a source post-fork overwrite never leaks in.
	Serve() error
	// FaultCount returns how many page faults Serve has filled so far (the child's
	// working-set size), the measure that the lazy import faults only a few MiB
	// instead of eagerly copying the whole guest RAM.
	FaultCount() int64
	// Close tears the handler down (closes the child uffd, unblocking Serve; munmaps
	// the source views; removes the socket). Idempotent.
	Close() error
}
