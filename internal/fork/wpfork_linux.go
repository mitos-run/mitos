//go:build linux

package fork

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// This file is the Linux side of the Mitos live copy-on-write (live-cow) fork
// handler (milestone m4b): the parent-side userfaultfd write-protect (UFFD_WP)
// engine. It receives the uffd the patched Firecracker created over its live
// guest mapping (SCM_RIGHTS over FIRECRACKER_MITOS_WP_UDS), FREEZES the guest at
// the fork point (UFFDIO_WRITEPROTECT the whole region), and serves the resulting
// write-protect faults with copy-before-unprotect into a private FROZEN memfd so
// a resumed parent can no longer leak a post-fork write into a co-located child.
// See wpfork.go for the full algorithm and correctness argument; this file is a
// faithful Go port of mitos-run/firecracker proof/wp_fork_proof.c `handler` mode.
//
// SECURITY (internal/fork is a named-reviewer path). The handler owns no policy:
// it copies pre-write pages the parent Firecracker's own uffd delivers into a
// frozen memfd it created, and unprotects. It reads the parent's guest memory
// read-only through /proc/<pid>/fd/<fd> (the m1 export the parent itself
// published) and writes nowhere on the host filesystem. The whole path is dark
// unless the LiveCowFork flag armed the parent with FIRECRACKER_MITOS_* env.

// UFFD write-protect ioctl and pagefault-flag constants (linux/userfaultfd.h,
// asm-generic ioctl encoding, same on x86_64 and aarch64). x/sys/unix ships no
// UFFD wrappers, so we define exactly what the WP handler uses.
//
//	UFFDIO_WRITEPROTECT = _IOWR(0xAA, 0x06, struct uffdio_writeprotect) with
//	sizeof(struct uffdio_writeprotect)==24:
//	  (dir=3<<30) | (size=24<<16) | (type=0xAA<<8) | (nr=0x06) = 0xc018aa06.
const (
	uffdioWriteprotect = 0xc018aa06
	// writeprotectModeWP arms write-protection (UFFDIO_WRITEPROTECT_MODE_WP,
	// 1<<0). Mode 0 removes write-protection and wakes any blocked writer.
	writeprotectModeWP = 0x1
	// uffdPagefaultFlagWP (UFFD_PAGEFAULT_FLAG_WP, 1<<1) tags a delivered fault as
	// a write-protect fault (vs a MISSING fault). The handler only services these.
	uffdPagefaultFlagWP = 0x2
)

// uffdioWriteprotectArg mirrors struct uffdio_writeprotect: a {start,len} range
// followed by the mode word.
type uffdioWriteprotectArg struct {
	Start uint64
	Len   uint64
	Mode  uint64
}

// frozenEpoch is the per-fork frozen state: one FROZEN image plus its 1-bit-per-page
// source selector, created FRESH at each Freeze (each ForkSnapshot). A co-located
// child of that fork reads its point-in-time guest memory from THIS epoch's memfds,
// so repeated forks of one source never share frozen state: fork B's child can never
// read a page frozen for fork A, and fork A's still-live children are never mutated
// by fork B or by a source write that happens after fork B (the m2 inheritance
// invariant across REPEATED forks). The handler keeps every epoch alive until Close,
// because an earlier fork's children may still reference that epoch's memfds through
// /proc while a later fork runs. The memfds are sparse (only clobbered pages consume
// RAM), so the cost is bounded by the pages actually rewritten, not by guest RAM per
// fork.
type frozenEpoch struct {
	frozenFd int
	frozen   []byte // private FROZEN image for this fork, RW MAP_SHARED
	// frozenBM is the 1-bit-per-page selector (set bit = a child of THIS fork must
	// read that page from this epoch's FROZEN image). It is a MAP_SHARED view of
	// frozenBMFd, a memfd, so a co-located child can reopen the SAME region through
	// /proc and read the CURRENT bits at attach time instead of a stale snapshot (the
	// m2 no-leak invariant end to end: a page frozen after the import is assembled but
	// before the child attaches is still sourced from FROZEN).
	frozenBMFd int
	frozenBM   []byte
}

// wpForkHandler owns the parent-side write-protect fork engine for one live-cow
// PARENT VM, ACROSS every fork of that parent. Lifecycle: newWPForkHandler (bind the
// UDS) -> Receive (accept the patched Firecracker's uffd + region layout, mmap the
// parent's live memfd) -> Freeze (allocate a FRESH frozenEpoch and arm WP at the fork
// point, then the caller resumes the parent; called once PER fork) -> Serve
// (goroutine, copy-before-unprotect for the life of the parent, fanning each fault
// into every live epoch) -> Close. The uffd is created once by Firecracker over the
// parent's live mapping and delivered once at Receive, so a fork does NOT get a fresh
// handler; instead each Freeze starts a fresh epoch so per-fork frozen state is
// independent while the single uffd + Serve loop is shared.
type wpForkHandler struct {
	cfg WPForkConfig
	ln  *net.UnixListener

	mu       sync.Mutex
	uffd     int
	regions  []uffdMapping
	live     []byte // parent guest memory, read-only MAP_SHARED via /proc
	liveSize uint64
	// epochs holds one frozenEpoch per Freeze (per fork), in fork order. The CURRENT
	// (last) epoch is the one a just-forked child imports; earlier epochs stay live so
	// prior forks' children keep reading their own point-in-time pages. Guarded by mu.
	epochs   []*frozenEpoch
	pageSize uint64
	closed   bool

	// uffdFile wraps the uffd so Serve's read is driven by the Go runtime poller:
	// a blocking read(2) on the raw uffd is not interrupted when another goroutine
	// closes it, but uffdFile.Close() unblocks a poller-driven Read cleanly. The
	// raw h.uffd is retained for UFFDIO_* ioctls (which ignore O_NONBLOCK); the
	// file OWNS the fd, so Close closes the file and never double-closes h.uffd.
	uffdFile *os.File

	// served counts write-protect faults the handler has copied-and-unprotected.
	served int64

	// freezeNanos records how long the fork-point UFFDIO_WRITEPROTECT-all took
	// (the parent-pause contributor the m2 design measures).
	freezeNanos int64
}

// newWPForkHandler binds the write-protect handshake unix socket the caller
// passes to the parent Firecracker as FIRECRACKER_MITOS_WP_UDS. It MUST be called
// (and the socket bound) BEFORE the parent Firecracker starts, because the
// patched Firecracker CONNECTS to this socket during guest-memory setup; a socket
// that is not yet listening makes the parent fall back to the m1 paused-parent
// contract (fail-closed).
func newWPForkHandler(cfg WPForkConfig) (*wpForkHandler, error) {
	if cfg.UDSPath == "" {
		return nil, fmt.Errorf("wpfork: empty WP UDS path")
	}
	if cfg.MemExportPath == "" {
		return nil, fmt.Errorf("wpfork: empty memfd export path")
	}
	// Remove a stale socket so the bind does not fail with EADDRINUSE after a
	// crashed prior fork (matches the restore-handler and control-socket cleanup).
	_ = os.Remove(cfg.UDSPath)
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: cfg.UDSPath, Net: "unix"})
	if err != nil {
		return nil, fmt.Errorf("wpfork: listen %s: %w", cfg.UDSPath, err)
	}
	return &wpForkHandler{cfg: cfg, ln: ln, uffd: -1}, nil
}

// StartWPForkHandler binds the write-protect handshake socket for a live-cow fork
// and returns the handler ready for Receive. It MUST be called before the parent
// Firecracker starts (the patched Firecracker connects to the socket during
// guest-memory setup).
func StartWPForkHandler(cfg WPForkConfig) (WPForkHandle, error) {
	return newWPForkHandler(cfg)
}

// Receive accepts the patched Firecracker's connection and completes the m2
// handshake: it reads the region layout (JSON body) and the userfaultfd
// (SCM_RIGHTS), then mmaps the parent's live guest memfd read-only (from the
// FIRECRACKER_MITOS_SHARED_MEM_EXPORT coordinates the parent published). It must be
// running concurrently with the parent Firecracker startup, which is what drives
// Firecracker to connect. After it returns the handler holds the uffd, the region
// table, and the parent's live memory view. The per-fork FROZEN image + bitmap are
// NOT created here: each Freeze (each fork) allocates its OWN epoch (newEpoch), so
// repeated forks of one source never share frozen state.
func (h *wpForkHandler) Receive() error {
	conn, err := h.ln.AcceptUnix()
	if err != nil {
		return fmt.Errorf("wpfork: accept: %w", err)
	}
	defer conn.Close()

	// The parent has connected, so it has already written the memfd export file
	// (mitos_export_shared_memfd runs before mitos_wp_offer in resources.rs). Read
	// it now to reach the parent's live guest memory.
	exportRaw, err := os.ReadFile(h.cfg.MemExportPath)
	if err != nil {
		return fmt.Errorf("wpfork: read memfd export %s: %w", h.cfg.MemExportPath, err)
	}
	exp, err := parseMemfdExport(string(exportRaw))
	if err != nil {
		return fmt.Errorf("wpfork: %w", err)
	}

	uffdFD, regions, err := recvUffdHandshake(conn)
	if err != nil {
		return err
	}

	live, err := mmapLiveMemfd(exp)
	if err != nil {
		_ = unix.Close(uffdFD)
		return err
	}

	pageSize := regions[0].pageSizeBytes()

	// Wrap the uffd in an *os.File so Serve's read runs on the Go runtime poller:
	// it delivers fault messages the same as a blocking read but, unlike a raw
	// read(2), Close (uffdFile.Close) unblocks it cleanly. os.NewFile switches the
	// fd to non-blocking and registers it with the poller; the raw h.uffd alias is
	// used only for UFFDIO_* ioctls, which ignore O_NONBLOCK.
	uffdFile := os.NewFile(uintptr(uffdFD), "wpfork-uffd")

	h.mu.Lock()
	h.uffd = uffdFD
	h.uffdFile = uffdFile
	h.regions = regions
	h.live = live
	h.liveSize = exp.bytes
	h.pageSize = pageSize
	h.mu.Unlock()
	return nil
}

// newEpoch allocates a FRESH per-fork frozen state: a private FROZEN memfd sized to
// the guest RAM and its 1-bit-per-page selector memfd, both zeroed. Freeze calls it
// once per fork, so each fork's children read that fork's point-in-time pages and an
// earlier fork's frozen bytes are never overwritten by a later fork or by a source
// write after the later fork (the m2 inheritance invariant across repeated forks).
// The memfds are sparse: only pages the source actually clobbers after this fork's
// freeze consume RAM.
func (h *wpForkHandler) newEpoch() (*frozenEpoch, error) {
	h.mu.Lock()
	bytes := h.liveSize
	pageSize := h.pageSize
	h.mu.Unlock()
	if bytes == 0 || pageSize == 0 {
		return nil, fmt.Errorf("wpfork: newEpoch before handshake")
	}
	frozenFd, err := unix.MemfdCreate(frozenMemfdName, unix.MFD_CLOEXEC)
	if err != nil {
		return nil, fmt.Errorf("wpfork: memfd_create frozen: %w", err)
	}
	if err := unix.Ftruncate(frozenFd, int64(bytes)); err != nil {
		_ = unix.Close(frozenFd)
		return nil, fmt.Errorf("wpfork: ftruncate frozen: %w", err)
	}
	frozen, err := unix.Mmap(frozenFd, 0, int(bytes), unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
	if err != nil {
		_ = unix.Close(frozenFd)
		return nil, fmt.Errorf("wpfork: mmap frozen: %w", err)
	}
	// The frozen bitmap is backed by a memfd (not MAP_SHARED|MAP_ANONYMOUS) so a
	// co-located child can reopen the SAME region through /proc/<pid>/fd and read the
	// CURRENT per-page selector at attach time. frozenBitmapName is the identity the
	// child verifies on reopen.
	bmBytes := frozenBitmapBytes(bytes, pageSize)
	frozenBMFd, err := unix.MemfdCreate(frozenBitmapName, unix.MFD_CLOEXEC)
	if err != nil {
		_ = unix.Munmap(frozen)
		_ = unix.Close(frozenFd)
		return nil, fmt.Errorf("wpfork: memfd_create frozen bitmap: %w", err)
	}
	if err := unix.Ftruncate(frozenBMFd, int64(bmBytes)); err != nil {
		_ = unix.Munmap(frozen)
		_ = unix.Close(frozenFd)
		_ = unix.Close(frozenBMFd)
		return nil, fmt.Errorf("wpfork: ftruncate frozen bitmap: %w", err)
	}
	frozenBM, err := unix.Mmap(frozenBMFd, 0, int(bmBytes), unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
	if err != nil {
		_ = unix.Munmap(frozen)
		_ = unix.Close(frozenFd)
		_ = unix.Close(frozenBMFd)
		return nil, fmt.Errorf("wpfork: mmap frozen bitmap: %w", err)
	}
	return &frozenEpoch{frozenFd: frozenFd, frozen: frozen, frozenBMFd: frozenBMFd, frozenBM: frozenBM}, nil
}

// currentEpoch returns the latest fork's frozen state (the one a just-forked child
// imports), or nil before the first Freeze. Caller must hold h.mu.
func (h *wpForkHandler) currentEpoch() *frozenEpoch {
	if len(h.epochs) == 0 {
		return nil
	}
	return h.epochs[len(h.epochs)-1]
}

// recvUffdHandshake reads the single SCM_RIGHTS message the patched Firecracker
// sends: the region mappings as the JSON body and the userfaultfd as the passed
// descriptor. Mirrors the restore-side handshake in uffd_linux.go.
func recvUffdHandshake(conn *net.UnixConn) (int, []uffdMapping, error) {
	raw, err := conn.SyscallConn()
	if err != nil {
		return -1, nil, fmt.Errorf("wpfork: syscallconn: %w", err)
	}
	buf := make([]byte, 1<<16)
	oob := make([]byte, 1024)
	var n, oobn int
	var rerr error
	if cerr := raw.Read(func(fd uintptr) bool {
		n, oobn, _, _, rerr = unix.Recvmsg(int(fd), buf, oob, 0)
		return true
	}); cerr != nil {
		return -1, nil, fmt.Errorf("wpfork: rawconn read: %w", cerr)
	}
	if rerr != nil {
		return -1, nil, fmt.Errorf("wpfork: recvmsg: %w", rerr)
	}
	scms, err := unix.ParseSocketControlMessage(oob[:oobn])
	if err != nil {
		return -1, nil, fmt.Errorf("wpfork: parse control message: %w", err)
	}
	gotFD := -1
	for _, scm := range scms {
		fds, perr := unix.ParseUnixRights(&scm)
		if perr != nil {
			continue
		}
		if len(fds) > 0 {
			gotFD = fds[0]
		}
	}
	if gotFD < 0 {
		return -1, nil, fmt.Errorf("wpfork: handshake carried no userfaultfd descriptor")
	}
	var regions []uffdMapping
	if err := json.Unmarshal(buf[:n], &regions); err != nil {
		_ = unix.Close(gotFD)
		return -1, nil, fmt.Errorf("wpfork: decode region mappings %q: %w", string(buf[:n]), err)
	}
	if len(regions) == 0 {
		_ = unix.Close(gotFD)
		return -1, nil, fmt.Errorf("wpfork: handshake carried no region mappings")
	}
	return gotFD, regions, nil
}

// mmapLiveMemfd re-opens the parent Firecracker's guest memfd through
// /proc/<pid>/fd/<fd> and maps it read-only MAP_SHARED, so the handler reads the
// parent's LIVE guest pages (still the fork-time bytes while a writer is blocked
// on a WP fault). This is the m1 export contract consumed on the handler side.
func mmapLiveMemfd(exp memfdExport) ([]byte, error) {
	path := fmt.Sprintf("/proc/%d/fd/%d", exp.pid, exp.fd)
	f, err := os.OpenFile(path, os.O_RDONLY, 0)
	if err != nil {
		return nil, fmt.Errorf("wpfork: open parent memfd %s: %w", path, err)
	}
	defer f.Close()
	live, err := unix.Mmap(int(f.Fd()), 0, int(exp.bytes), unix.PROT_READ, unix.MAP_SHARED)
	if err != nil {
		return nil, fmt.Errorf("wpfork: mmap parent memfd: %w", err)
	}
	return live, nil
}

// Freeze starts a NEW fork: it allocates a fresh per-fork frozen epoch (so this
// fork's children never inherit a page frozen for an earlier fork) and arms
// write-protection over the whole guest region at the fork point. After it returns
// every guest page is write-protected in the parent's mapping, so the caller may
// RESUME the parent: any page the parent then writes takes a WP fault that Serve
// resolves copy-before-unprotect, fanning the fork-time bytes into every live epoch
// that has not yet captured that page. Called once per ForkSnapshot; its duration is
// recorded and returned (the m2 parent-pause contributor).
func (h *wpForkHandler) Freeze() (time.Duration, error) {
	h.mu.Lock()
	uffd := h.uffd
	regions := h.regions
	h.mu.Unlock()
	if uffd < 0 {
		return 0, fmt.Errorf("wpfork: Freeze before handshake")
	}
	// Allocate the fresh epoch BEFORE arming WP, but publish it (append) only after
	// the region is protected: no fault can fire during the freeze (the source is
	// paused across the fork window), and a WP failure then leaves no dangling epoch.
	ep, err := h.newEpoch()
	if err != nil {
		return 0, err
	}
	start := time.Now()
	for _, r := range regions {
		if err := h.writeprotect(r.BaseHostVirtAddr, r.Size, true); err != nil {
			ep.close()
			return 0, fmt.Errorf("wpfork: freeze region [%#x,+%#x): %w", r.BaseHostVirtAddr, r.Size, err)
		}
	}
	d := time.Since(start)
	h.mu.Lock()
	h.epochs = append(h.epochs, ep)
	h.mu.Unlock()
	atomic.StoreInt64(&h.freezeNanos, d.Nanoseconds())
	return d, nil
}

// close munmaps and closes the epoch's memfds. Idempotent-ish: called on Freeze
// rollback and on handler Close.
func (e *frozenEpoch) close() {
	if e.frozen != nil {
		_ = unix.Munmap(e.frozen)
		e.frozen = nil
	}
	if e.frozenBM != nil {
		_ = unix.Munmap(e.frozenBM)
		e.frozenBM = nil
	}
	if e.frozenBMFd >= 0 {
		_ = unix.Close(e.frozenBMFd)
		e.frozenBMFd = -1
	}
	if e.frozenFd >= 0 {
		_ = unix.Close(e.frozenFd)
		e.frozenFd = -1
	}
}

// writeprotect arms (protect=true) or removes (protect=false) write-protection
// over [addr,addr+len). Removing WP also wakes any writer blocked on that range.
func (h *wpForkHandler) writeprotect(addr, length uint64, protect bool) error {
	arg := uffdioWriteprotectArg{Start: addr, Len: length}
	if protect {
		arg.Mode = writeprotectModeWP
	}
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(h.uffd), uintptr(uffdioWriteprotect), uintptr(unsafe.Pointer(&arg)))
	if errno != 0 {
		return fmt.Errorf("UFFDIO_WRITEPROTECT addr=%#x len=%#x protect=%v: %w", addr, length, protect, errno)
	}
	return nil
}

// Serve runs the write-protect fault loop until Close. On each WP fault it copies
// the faulting page's pre-write (fork-time) bytes from the live guest memory into
// the FROZEN image, marks the page frozen in the shared bitmap, and only THEN
// unprotects the page and wakes the parent's writer. Ordering (copy before
// unprotect) is the whole correctness argument (wpfork.go). It returns when the
// uffd is closed (Close) or on a fatal read error.
func (h *wpForkHandler) Serve() error {
	h.mu.Lock()
	uffdFile := h.uffdFile
	h.mu.Unlock()
	if uffdFile == nil {
		return fmt.Errorf("wpfork: Serve before handshake")
	}
	var msg uffdMsg
	msgBuf := (*[unsafe.Sizeof(msg)]byte)(unsafe.Pointer(&msg))[:]
	for {
		// Poller-driven read: blocks until a fault message is ready, and returns a
		// clean ErrClosed when Close closes uffdFile (a raw read(2) would hang).
		nread, err := uffdFile.Read(msgBuf)
		if err != nil {
			if errors.Is(err, os.ErrClosed) {
				return nil
			}
			h.mu.Lock()
			closed := h.closed
			h.mu.Unlock()
			if closed {
				return nil
			}
			return fmt.Errorf("wpfork: read uffd: %w", err)
		}
		if nread != int(unsafe.Sizeof(msg)) || msg.Event != uffdEventPagefault {
			continue
		}
		if msg.PfFlags&uffdPagefaultFlagWP == 0 {
			// Not a write-protect fault: nothing was registered MISSING, so this
			// should not happen. Ignore rather than mis-serve.
			continue
		}
		if err := h.serveFault(msg.PfAddress); err != nil {
			return err
		}
	}
}

// serveFault handles one write-protect fault at host address addr: copy the page's
// fork-time bytes into EVERY live epoch that has not yet captured that page, mark it
// in each, then unprotect + wake the writer. Fanning into every uncaptured epoch is
// what keeps concurrent forks independent: the page's pre-write value is the
// fork-time value for every epoch whose freeze happened before this write and that
// has not seen a write to this page since (bit clear). An epoch that already froze
// the page (bit set) is skipped, so an earlier fork's frozen bytes are never
// overwritten by a write that happens after a later fork.
// dumpRegions renders the registered region layout compactly for diagnostics: the
// base host virtual address, size, and mem-file offset of every region the WP
// handshake advertised. It is used only in error context, so it never runs on the
// hot fault path.
func dumpRegions(regions []uffdMapping) string {
	parts := make([]string, 0, len(regions))
	for i, r := range regions {
		parts = append(parts, fmt.Sprintf("[%d base=%#x size=%#x off=%#x]", i, r.BaseHostVirtAddr, r.Size, r.Offset))
	}
	return strings.Join(parts, " ")
}

func (h *wpForkHandler) serveFault(addr uint64) error {
	h.mu.Lock()
	regions := h.regions
	live := h.live
	epochs := make([]*frozenEpoch, len(h.epochs))
	copy(epochs, h.epochs)
	pageSize := h.pageSize
	h.mu.Unlock()

	pageBase, fileOffset, ok := fileOffsetForAddr(regions, addr, pageSize)
	if !ok {
		// A fault outside the registered range: skip it (defensive).
		return nil
	}
	if fileOffset+pageSize > uint64(len(live)) {
		return fmt.Errorf("wpfork: fault page [%#x,+%#x) past mapped memory (addr=%#x live=%d regions=%s)", fileOffset, pageSize, addr, len(live), dumpRegions(regions))
	}
	pageIdx := fileOffset / pageSize
	// COPY-BEFORE-UNPROTECT: the writer is blocked on this WP fault, so live[fileOffset]
	// still holds the fork-time bytes. Preserve them for the children of every fork
	// that still needs this page, mark it, THEN unprotect so the parent's write may
	// land. All copies complete before the single unprotect, so no epoch can observe
	// the parent's new value while a fork-time value is still owed to it.
	for _, ep := range epochs {
		if fileOffset+pageSize > uint64(len(ep.frozen)) {
			return fmt.Errorf("wpfork: fault page [%#x,+%#x) past frozen image (frozen=%d)", fileOffset, pageSize, len(ep.frozen))
		}
		if testFrozenBit(ep.frozenBM, pageIdx) {
			continue // this fork already captured the page's fork-time bytes
		}
		copy(ep.frozen[fileOffset:fileOffset+pageSize], live[fileOffset:fileOffset+pageSize])
		setFrozenBit(ep.frozenBM, pageIdx)
	}
	if err := h.writeprotect(pageBase, pageSize, false); err != nil {
		return fmt.Errorf("wpfork: unprotect+wake: %w", err)
	}
	atomic.AddInt64(&h.served, 1)
	return nil
}

// FrozenFd returns the descriptor of the CURRENT fork's private FROZEN memfd. -1
// before the first Freeze (each Freeze starts a fresh epoch).
func (h *wpForkHandler) FrozenFd() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	ep := h.currentEpoch()
	if ep == nil {
		return -1
	}
	return ep.frozenFd
}

// FrozenPage reports whether page pageIndex has been copied into the CURRENT fork's
// FROZEN image (the per-page source selector a co-located child of this fork
// consults). False before the first Freeze.
func (h *wpForkHandler) FrozenPage(pageIndex uint64) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	ep := h.currentEpoch()
	if ep == nil {
		return false
	}
	return testFrozenBit(ep.frozenBM, pageIndex)
}

// FrozenBitmap returns a copy of the CURRENT fork's frozen bitmap at the instant of
// the call, so a co-located child reads a stable per-page source selector while
// Serve keeps marking pages. Nil before the first Freeze.
func (h *wpForkHandler) FrozenBitmap() []byte {
	h.mu.Lock()
	defer h.mu.Unlock()
	ep := h.currentEpoch()
	if ep == nil || ep.frozenBM == nil {
		return nil
	}
	out := make([]byte, len(ep.frozenBM))
	copy(out, ep.frozenBM)
	return out
}

// ChildImport assembles the coordinates a co-located fork child needs to boot from
// the parent's live memory (see ChildImportProvider). It reads the parent's m1
// export to reach the parent shared memfd, and points the child at this handler's
// FROZEN memfd and its LIVE frozen bitmap memfd (both owned by THIS process). The
// bitmap is passed as a live memfd, NOT a static file copy, so the child reads the
// CURRENT per-page selector at attach time: a page this handler freezes AFTER
// ChildImport runs but BEFORE the child attaches is still sourced from FROZEN, so
// the resumed parent's post-fork write can never leak into the child.
//
// Each descriptor's identity (st_ino, st_dev) is captured here (the parent owns the
// FROZEN and bitmap memfds directly; the guest memfd it reaches once through /proc)
// so the child can verify on reopen that a recycled PID has not handed it a foreign
// fd. dir is the child's node-local snapshot dir; it is validated as the trust
// boundary but the import no longer writes any file there.
func (h *wpForkHandler) ChildImport(dir string) (ChildMemfdImport, error) {
	h.mu.Lock()
	ep := h.currentEpoch()
	frozenFd, frozenBMFd := -1, -1
	if ep != nil {
		frozenFd = ep.frozenFd
		frozenBMFd = ep.frozenBMFd
	}
	liveSize := h.liveSize
	pageSize := h.pageSize
	h.mu.Unlock()
	if frozenFd < 0 || frozenBMFd < 0 || liveSize == 0 || pageSize == 0 {
		return ChildMemfdImport{}, fmt.Errorf("wpfork: ChildImport before Freeze")
	}
	if dir == "" {
		return ChildMemfdImport{}, fmt.Errorf("wpfork: ChildImport empty dir")
	}
	exportRaw, err := os.ReadFile(h.cfg.MemExportPath)
	if err != nil {
		return ChildMemfdImport{}, fmt.Errorf("wpfork: ChildImport read memfd export %s: %w", h.cfg.MemExportPath, err)
	}
	exp, err := parseMemfdExport(string(exportRaw))
	if err != nil {
		return ChildMemfdImport{}, fmt.Errorf("wpfork: ChildImport %w", err)
	}
	parentIno, parentDev, err := procFdIdentity(exp.pid, exp.fd)
	if err != nil {
		return ChildMemfdImport{}, fmt.Errorf("wpfork: ChildImport identify parent memfd: %w", err)
	}
	frozenIno, frozenDev, err := fdIdentity(frozenFd)
	if err != nil {
		return ChildMemfdImport{}, fmt.Errorf("wpfork: ChildImport identify frozen memfd: %w", err)
	}
	bmIno, bmDev, err := fdIdentity(frozenBMFd)
	if err != nil {
		return ChildMemfdImport{}, fmt.Errorf("wpfork: ChildImport identify frozen bitmap memfd: %w", err)
	}
	return ChildMemfdImport{
		ParentPID: exp.pid,
		ParentFD:  exp.fd,
		ParentIno: parentIno,
		ParentDev: parentDev,
		Bytes:     exp.bytes,
		FrozenPID: os.Getpid(),
		FrozenFD:  frozenFd,
		FrozenIno: frozenIno,
		FrozenDev: frozenDev,
		BitmapPID: os.Getpid(),
		BitmapFD:  frozenBMFd,
		BitmapIno: bmIno,
		BitmapDev: bmDev,
		PageSize:  pageSize,
	}, nil
}

// FaultCount returns the number of write-protect faults Serve has resolved.
func (h *wpForkHandler) FaultCount() int64 { return atomic.LoadInt64(&h.served) }

// FreezeDuration returns the recorded fork-point freeze duration.
func (h *wpForkHandler) FreezeDuration() time.Duration {
	return time.Duration(atomic.LoadInt64(&h.freezeNanos))
}

// Close tears down the handler: it wakes Serve's poll via the stop pipe, closes
// the uffd, munmaps the memory views, and removes the socket. Idempotent.
func (h *wpForkHandler) Close() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return nil
	}
	h.closed = true
	if h.ln != nil {
		_ = h.ln.Close()
	}
	// Closing the file unblocks Serve's poller-driven Read and closes the uffd fd
	// it owns. Clear the raw alias without a second unix.Close (double close).
	if h.uffdFile != nil {
		_ = h.uffdFile.Close()
		h.uffdFile = nil
		h.uffd = -1
	} else if h.uffd >= 0 {
		_ = unix.Close(h.uffd)
		h.uffd = -1
	}
	if h.live != nil {
		_ = unix.Munmap(h.live)
		h.live = nil
	}
	// Tear down every fork's epoch: an earlier fork's children may have referenced
	// its memfds through /proc, but Close means the whole parent path is going away.
	for _, ep := range h.epochs {
		ep.close()
	}
	h.epochs = nil
	_ = os.Remove(h.cfg.UDSPath)
	return nil
}
