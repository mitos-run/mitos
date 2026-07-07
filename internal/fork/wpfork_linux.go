//go:build linux

package fork

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
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

// wpForkHandler owns the parent-side write-protect fork engine for one live-cow
// fork. Lifecycle: newWPForkHandler (bind the UDS) -> receive (accept the patched
// Firecracker's uffd + region layout, mmap the parent's live memfd, create the
// frozen memfd) -> Freeze (arm WP at the fork point, then the caller resumes the
// parent) -> Serve (goroutine, copy-before-unprotect for the life of the fork) ->
// Close.
type wpForkHandler struct {
	cfg WPForkConfig
	ln  *net.UnixListener

	mu       sync.Mutex
	uffd     int
	regions  []uffdMapping
	live     []byte // parent guest memory, read-only MAP_SHARED via /proc
	liveSize uint64
	frozenFd int
	frozen   []byte // private FROZEN image, RW MAP_SHARED
	frozenBM []byte // 1 bit/page: which pages a child must read from FROZEN
	pageSize uint64
	closed   bool

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
	return &wpForkHandler{cfg: cfg, ln: ln, uffd: -1, frozenFd: -1}, nil
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
// FIRECRACKER_MITOS_SHARED_MEM_EXPORT coordinates the parent published) and
// creates the private FROZEN memfd the copy-before-unprotect loop writes into.
// It must be running concurrently with the parent Firecracker startup, which is
// what drives Firecracker to connect. After it returns the handler holds the
// uffd, the region table, and both memory views.
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
	frozenFd, err := unix.MemfdCreate("mitos-frozen", unix.MFD_CLOEXEC)
	if err != nil {
		_ = unix.Close(uffdFD)
		_ = unix.Munmap(live)
		return fmt.Errorf("wpfork: memfd_create frozen: %w", err)
	}
	if err := unix.Ftruncate(frozenFd, int64(exp.bytes)); err != nil {
		_ = unix.Close(uffdFD)
		_ = unix.Munmap(live)
		_ = unix.Close(frozenFd)
		return fmt.Errorf("wpfork: ftruncate frozen: %w", err)
	}
	frozen, err := unix.Mmap(frozenFd, 0, int(exp.bytes), unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
	if err != nil {
		_ = unix.Close(uffdFD)
		_ = unix.Munmap(live)
		_ = unix.Close(frozenFd)
		return fmt.Errorf("wpfork: mmap frozen: %w", err)
	}
	bmBytes := frozenBitmapBytes(exp.bytes, pageSize)
	frozenBM, err := unix.Mmap(-1, 0, int(bmBytes), unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED|unix.MAP_ANONYMOUS)
	if err != nil {
		_ = unix.Close(uffdFD)
		_ = unix.Munmap(live)
		_ = unix.Munmap(frozen)
		_ = unix.Close(frozenFd)
		return fmt.Errorf("wpfork: mmap frozen bitmap: %w", err)
	}

	// Serve with blocking reads; Close closes the uffd to unblock the loop.
	_ = unix.SetNonblock(uffdFD, false)

	h.mu.Lock()
	h.uffd = uffdFD
	h.regions = regions
	h.live = live
	h.liveSize = exp.bytes
	h.frozenFd = frozenFd
	h.frozen = frozen
	h.frozenBM = frozenBM
	h.pageSize = pageSize
	h.mu.Unlock()
	return nil
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

// Freeze arms write-protection over the whole guest region at the fork point.
// After it returns every guest page is write-protected in the parent's mapping,
// so the caller may RESUME the parent: any page the parent then writes takes a WP
// fault that Serve resolves copy-before-unprotect. This is the m2 parent-pause
// contributor; its duration is recorded and returned.
func (h *wpForkHandler) Freeze() (time.Duration, error) {
	h.mu.Lock()
	uffd := h.uffd
	regions := h.regions
	h.mu.Unlock()
	if uffd < 0 {
		return 0, fmt.Errorf("wpfork: Freeze before handshake")
	}
	start := time.Now()
	for _, r := range regions {
		if err := h.writeprotect(r.BaseHostVirtAddr, r.Size, true); err != nil {
			return 0, fmt.Errorf("wpfork: freeze region [%#x,+%#x): %w", r.BaseHostVirtAddr, r.Size, err)
		}
	}
	d := time.Since(start)
	atomic.StoreInt64(&h.freezeNanos, d.Nanoseconds())
	return d, nil
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
	uffd := h.uffd
	h.mu.Unlock()
	if uffd < 0 {
		return fmt.Errorf("wpfork: Serve before handshake")
	}
	var msg uffdMsg
	msgBuf := (*[unsafe.Sizeof(msg)]byte)(unsafe.Pointer(&msg))[:]
	for {
		nread, err := unix.Read(uffd, msgBuf)
		if err != nil {
			if err == unix.EINTR {
				continue
			}
			// A closed uffd (Close) returns EBADF/errno; treat as clean shutdown.
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

// serveFault handles one write-protect fault at host address addr: copy the
// page's fork-time bytes into FROZEN, mark it, then unprotect + wake the writer.
func (h *wpForkHandler) serveFault(addr uint64) error {
	h.mu.Lock()
	regions := h.regions
	live := h.live
	frozen := h.frozen
	bm := h.frozenBM
	pageSize := h.pageSize
	h.mu.Unlock()

	pageBase, fileOffset, ok := fileOffsetForAddr(regions, addr, pageSize)
	if !ok {
		// A fault outside the registered range: skip it (defensive).
		return nil
	}
	if fileOffset+pageSize > uint64(len(live)) || fileOffset+pageSize > uint64(len(frozen)) {
		return fmt.Errorf("wpfork: fault page [%#x,+%#x) past mapped memory", fileOffset, pageSize)
	}
	// COPY-BEFORE-WRITE: the writer is blocked on this WP fault, so live[fileOffset]
	// still holds the fork-time bytes. Preserve them for the children, mark the
	// page, THEN unprotect so the parent's write may land.
	copy(frozen[fileOffset:fileOffset+pageSize], live[fileOffset:fileOffset+pageSize])
	setFrozenBit(bm, fileOffset/pageSize)
	if err := h.writeprotect(pageBase, pageSize, false); err != nil {
		return fmt.Errorf("wpfork: unprotect+wake: %w", err)
	}
	atomic.AddInt64(&h.served, 1)
	return nil
}

// FrozenFd returns the descriptor of the private FROZEN memfd. -1 before Receive.
func (h *wpForkHandler) FrozenFd() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.frozenFd
}

// FrozenPage reports whether page pageIndex has been copied into the FROZEN
// image (the per-page source selector a co-located child consults).
func (h *wpForkHandler) FrozenPage(pageIndex uint64) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return testFrozenBit(h.frozenBM, pageIndex)
}

// FaultCount returns the number of write-protect faults Serve has resolved.
func (h *wpForkHandler) FaultCount() int64 { return atomic.LoadInt64(&h.served) }

// FreezeDuration returns the recorded fork-point freeze duration.
func (h *wpForkHandler) FreezeDuration() time.Duration {
	return time.Duration(atomic.LoadInt64(&h.freezeNanos))
}

// Close tears down the handler: it closes the uffd (unblocking Serve), munmaps
// the memory views, and removes the socket. Idempotent.
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
	if h.uffd >= 0 {
		_ = unix.Close(h.uffd)
		h.uffd = -1
	}
	if h.live != nil {
		_ = unix.Munmap(h.live)
		h.live = nil
	}
	if h.frozen != nil {
		_ = unix.Munmap(h.frozen)
		h.frozen = nil
	}
	if h.frozenBM != nil {
		_ = unix.Munmap(h.frozenBM)
		h.frozenBM = nil
	}
	if h.frozenFd >= 0 {
		_ = unix.Close(h.frozenFd)
		h.frozenFd = -1
	}
	_ = os.Remove(h.cfg.UDSPath)
	return nil
}
