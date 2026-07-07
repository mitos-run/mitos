package fork

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrLiveCowUnsupported is returned by StartWPForkHandler off Linux (the handler
// needs userfaultfd write-protect, a Linux-only mechanism). The live-cow fork
// path fails closed to the disk-snapshot restore in that case.
var ErrLiveCowUnsupported = errors.New("fork: live copy-on-write fork requires Linux userfaultfd write-protect")

// WPForkHandle is the parent-side live-cow fork engine the husk arms when the
// LiveCowFork flag is on. It is created bound to its UDS (StartWPForkHandler)
// BEFORE the parent Firecracker starts; Receive completes the SCM_RIGHTS
// handshake once Firecracker connects; Freeze arms write-protection at the fork
// point (after which the caller resumes the parent); Serve runs the
// copy-before-unprotect fault loop for the life of the fork. The Linux
// implementation is wpForkHandler (wpfork_linux.go); off Linux StartWPForkHandler
// returns ErrLiveCowUnsupported and nothing here runs.
type WPForkHandle interface {
	// Receive accepts the patched Firecracker's connection and reads the uffd +
	// region layout, mmaps the parent's live guest memfd, and creates the FROZEN
	// image. Run it concurrently with the parent Firecracker startup.
	Receive() error
	// Freeze write-protects the whole guest region at the fork point and returns
	// how long that took (the parent-pause contributor). Resume the parent after.
	Freeze() (time.Duration, error)
	// Serve runs the write-protect fault loop until Close.
	Serve() error
	// FrozenFd returns the descriptor of the private FROZEN memfd a co-located
	// child MAP_PRIVATEs (with the frozen bitmap) to read clobbered pages at their
	// fork-time value. -1 before Receive.
	FrozenFd() int
	// FrozenPage reports whether the handler has copied page pageIndex into the
	// FROZEN image (the parent clobbered it after the fork point). A co-located
	// child consults this per-page source selector: a frozen page is read from the
	// FROZEN memfd (fork-time value), an unfrozen page from the live shared memfd.
	FrozenPage(pageIndex uint64) bool
	// FrozenBitmap returns a COPY of the 1-bit-per-page frozen bitmap at the instant
	// of the call: bit p set means a co-located child must source page p from the
	// FROZEN memfd. A copy so the caller reads a stable snapshot while Serve keeps
	// marking pages. Nil before Receive.
	FrozenBitmap() []byte
	// ChildImport writes the current frozen bitmap into dir and returns the
	// coordinates a co-located fork child needs to boot its guest RAM from the
	// parent's live memory: the parent shared memfd (from the m1 export), this
	// handler's FROZEN memfd, that bitmap file, and the page size. It is the
	// ChildImportProvider the husk consults on a live-cow child spawn. Fails before
	// Receive (no memfd/bitmap yet).
	ChildImport(dir string) (ChildMemfdImport, error)
	// FaultCount returns how many write-protect faults Serve has resolved.
	FaultCount() int64
	// FreezeDuration returns the recorded fork-point freeze duration.
	FreezeDuration() time.Duration
	// Close tears down the handler (closes the uffd, munmaps, removes the socket).
	Close() error
}

// This file holds the platform-independent core of the Mitos live copy-on-write
// (live-cow) fork handler: the parent-side userfaultfd write-protect (UFFD_WP)
// engine that makes a RESUMED parent safe to share its live resident guest
// memory with co-located fork children (milestone m4b).
//
// Why this exists. The patched Firecracker (mitos-run/firecracker branch
// mitos/uffd-wp-v1.15.0) can, when FIRECRACKER_MITOS_SHARED_MEM is set, back the
// running guest RAM with a MAP_SHARED memfd so a peer process can MAP_PRIVATE the
// same fd and get kernel copy-on-write over the parent's live pages (m1). But
// MAP_PRIVATE alone does not FREEZE the parent: a resumed parent that writes a
// page a child has not yet copied would leak that post-fork write forward into
// the child. m2 closes that hole with UFFD write-protect: when
// FIRECRACKER_MITOS_WP_UDS names a unix socket, the patched Firecracker creates a
// userfaultfd over the live guest mapping (requesting
// UFFD_FEATURE_PAGEFAULT_FLAG_WP), registers every region in write-protect mode,
// and hands the uffd plus the region layout to an EXTERNAL handler over that
// socket via SCM_RIGHTS. This file (plus wpfork_linux.go) is that handler, on the
// forkd/husk side, ported faithfully from the m2 reference C handler
// (mitos-run/firecracker proof/wp_fork_proof.c, `handler` mode).
//
// The freeze/copy/unprotect algorithm (the whole correctness argument):
//  1. At the fork point the handler UFFDIO_WRITEPROTECTs the whole guest region
//     (freeze). Every guest page is now write-protected in the parent's mapping.
//  2. The parent resumes. When it writes a still-protected page the writing vCPU
//     thread takes a WP page-fault and BLOCKS in the kernel; the new value has
//     NOT landed yet.
//  3. The handler reads the fault, copies the page's pre-write (fork-time) bytes
//     into a FROZEN image and marks the page frozen, and only THEN issues
//     UFFDIO_WRITEPROTECT(mode=0) to unprotect the page and wake the writer. The
//     write now lands in the live image, but the fork-time bytes are safe.
//  4. A co-located child that reads that page is served from FROZEN (it was
//     clobbered) and so sees the fork-time value; a child that reads a page the
//     parent never wrote is served live from the shared memfd, which still holds
//     the fork-time value (cheap CoW). Either way the child sees a
//     point-in-time-T image.
//
// The ordering freeze -> copy -> unprotect is the invariant: because the copy
// completes before the unprotect, there is no window in which the parent's new
// value is visible while the fork-time value is lost. This is the same per-page
// source selection Firecracker's restore-side UFFD handler already performs
// (uffd_linux.go), extended with the running-parent WP registration that the m2
// Firecracker patch arms.
//
// SECURITY (threat-model: live-cow fork path). The handler receives a userfaultfd
// the patched Firecracker created over ITS OWN guest mapping; it never registers
// ranges or changes guest visibility, it only preserves pre-write pages and
// unprotects. Its only external input is the region layout Firecracker itself
// sends over a private per-VM unix socket, plus the parent's own exported memfd
// coordinates (read-only). It writes ONLY into a private frozen memfd it created.
// It performs no host-path write and copies no secret out of the guest. The whole
// path is gated OFF by default (the LiveCowFork flag); with the flag off none of
// this runs and the co-located fork path is byte-for-byte the disk-snapshot
// restore. See docs/threat-model.md and docs/fork-correctness.md.

// Environment variables the patched Firecracker reads. Setting these on the
// PARENT Firecracker process turns on the live-cow path; leaving them unset keeps
// the stock (disk-snapshot) behavior, so the patched binary is behavior-identical
// to stock until they are set. Names must match mitos-run/firecracker exactly
// (src/vmm/src/vstate/memory.rs and src/vmm/src/resources.rs).
const (
	// EnvSharedMem, when non-empty, makes Firecracker back the running guest RAM
	// with a MAP_SHARED memfd (m1) so a peer can MAP_PRIVATE it for CoW forks.
	EnvSharedMem = "FIRECRACKER_MITOS_SHARED_MEM"
	// EnvSharedMemExport names a file Firecracker writes the memfd coordinates to
	// as "<pid> <fd> <bytes>\n" (m1 export), which the handler reads to mmap the
	// parent's live guest memory read-only via /proc/<pid>/fd/<fd>.
	EnvSharedMemExport = "FIRECRACKER_MITOS_SHARED_MEM_EXPORT"
	// EnvWPUDS names the unix socket the handler LISTENS on and the patched
	// Firecracker CONNECTS to, over which Firecracker sends the uffd + region
	// layout via SCM_RIGHTS (m2).
	EnvWPUDS = "FIRECRACKER_MITOS_WP_UDS"
)

// WPForkConfig configures the parent-side live-cow fork handler. UDSPath is the
// unix socket the handler listens on and that the parent Firecracker is launched
// with as FIRECRACKER_MITOS_WP_UDS. MemExportPath is the file the parent
// Firecracker writes its memfd coordinates to (FIRECRACKER_MITOS_SHARED_MEM_EXPORT);
// the handler reads it to find the parent's live guest memory.
type WPForkConfig struct {
	UDSPath       string
	MemExportPath string
}

// LiveCowParentEnv returns the environment entries a co-located live-cow PARENT
// Firecracker must be launched with so it exports its guest memfd (m1) and offers
// the write-protect uffd to the handler listening on wpUDS (m2). Appending these
// to the Firecracker process environment is the ONLY thing that switches the
// patched binary from stock behavior to the live-cow path; an empty slice (the
// zero flag) leaves it stock. Kept pure so the wiring is testable off Linux.
func LiveCowParentEnv(wpUDS, memExport string) []string {
	if wpUDS == "" || memExport == "" {
		return nil
	}
	return []string{
		EnvSharedMem + "=1",
		EnvSharedMemExport + "=" + memExport,
		EnvWPUDS + "=" + wpUDS,
	}
}

// memfdExport is the parsed content of the parent Firecracker's
// FIRECRACKER_MITOS_SHARED_MEM_EXPORT file ("<pid> <fd> <bytes>"): the pid and fd
// through which the handler reaches the live guest memfd (as /proc/<pid>/fd/<fd>)
// and the total guest memory size in bytes.
type memfdExport struct {
	pid   int
	fd    int
	bytes uint64
}

// parseMemfdExport parses the "<pid> <fd> <bytes>" line the patched Firecracker
// writes to FIRECRACKER_MITOS_SHARED_MEM_EXPORT. Pure so it is unit tested off
// Linux; the /proc mmap that consumes it is Linux-only (wpfork_linux.go).
func parseMemfdExport(s string) (memfdExport, error) {
	var e memfdExport
	n, err := fmt.Sscanf(strings.TrimSpace(s), "%d %d %d", &e.pid, &e.fd, &e.bytes)
	if err != nil || n != 3 {
		return memfdExport{}, fmt.Errorf("parse memfd export %q: want \"<pid> <fd> <bytes>\": %w", s, err)
	}
	if e.pid <= 0 || e.fd < 0 || e.bytes == 0 {
		return memfdExport{}, fmt.Errorf("parse memfd export %q: non-positive field", s)
	}
	return e, nil
}

// frozenBitmapBytes returns the byte length of a 1-bit-per-page frozen bitmap for
// a guest memory of the given size and page size. The bitmap records which pages
// the handler has copied into the FROZEN image so a co-located child knows to
// source those pages from FROZEN instead of the live shared memfd (the same
// per-page source selection the restore-side handler performs). Pure; unit
// tested off Linux.
func frozenBitmapBytes(memSize, pageSize uint64) uint64 {
	if pageSize == 0 {
		return 0
	}
	npages := (memSize + pageSize - 1) / pageSize
	return (npages + 7) / 8
}

// setFrozenBit marks page index i frozen in bm. Out-of-range indices are ignored
// (defensive: a fault outside the mapped range is a fault the handler skips).
func setFrozenBit(bm []byte, i uint64) {
	byteIdx := i / 8
	if byteIdx >= uint64(len(bm)) {
		return
	}
	bm[byteIdx] |= 1 << (i % 8)
}

// testFrozenBit reports whether page index i is marked frozen in bm.
func testFrozenBit(bm []byte, i uint64) bool {
	byteIdx := i / 8
	if byteIdx >= uint64(len(bm)) {
		return false
	}
	return bm[byteIdx]&(1<<(i%8)) != 0
}
