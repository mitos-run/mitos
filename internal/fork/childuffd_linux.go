//go:build linux

package fork

import (
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"unsafe"

	"golang.org/x/sys/unix"
)

// This is the Linux side of the lazy live-cow child-side UFFD import (see
// childuffd.go for the design and the fork-latency argument). The handler binds a
// unix socket the child Firecracker's stock Uffd restore backend connects to,
// receives the child's userfaultfd + region layout, and fills each faulting guest
// page ON DEMAND by composing the SAME per-page source selection
// composeChildGuestMemory does eagerly, but PER FAULT and with a fault-time frozen
// re-check so a resumed source's post-fork write never leaks into the child.
//
// It reads the parent's shared memfd, the handler's FROZEN memfd, and the LIVE
// frozen bitmap memfd through the identity-verified /proc reopen the eager import
// already uses (openProcFdVerified), so it inherits the same PID-reuse fail-closed
// guarantees. It writes nowhere on the host filesystem: it only UFFDIO_COPYs
// composed source bytes into the guest RAM Firecracker registered.

// childUFFDHandler owns the lazy UFFD backend for one co-located fork child.
// Lifecycle: StartChildUFFDHandler (open+mmap the source/frozen/bitmap views, bind
// the socket) -> Receive (accept the child Firecracker, read its uffd + regions)
// -> Serve (goroutine, compose-and-fill faults for the life of the child) -> Close.
type childUFFDHandler struct {
	sockPath string
	ln       *net.UnixListener

	// Source views, all mapped for the whole guest RAM (import.Bytes). live is the
	// parent's shared memfd (a resumed source keeps writing it; a not-yet-frozen page
	// still holds its fork-time value because the WP handler freezes before a write
	// lands); frozen holds the fork-time bytes of every page the WP handler froze;
	// bm is the LIVE 1-bit-per-page selector (bit set = source that page from frozen).
	live   []byte
	frozen []byte
	bm     []byte

	bytes    uint64
	pageSize uint64

	// stage is a private anonymous page the Serve loop composes into before the
	// UFFDIO_COPY, so the copy source is a single stable off-heap address (never a
	// moving Go slice) and the compose can override a live page with the frozen page
	// atomically from Firecracker's point of view (UFFDIO_COPY fills a page once).
	stage []byte

	mu      sync.Mutex
	uffd    int
	regions []uffdMapping
	closed  bool

	// uffdFile wraps the child uffd so Serve's read is driven by the Go runtime
	// poller: a blocking read(2) on the raw uffd is not interrupted when Close closes
	// it, but uffdFile.Close() unblocks a poller-driven Read cleanly. The raw uffd is
	// retained for the UFFDIO_COPY ioctl (which ignores O_NONBLOCK).
	uffdFile *os.File

	// served counts the page faults Serve has filled (the child's working set).
	served int64
}

// StartChildUFFDHandler opens + identity-verifies + mmaps the three source views a
// co-located fork child needs (the parent shared memfd, the FROZEN memfd, and the
// LIVE frozen bitmap memfd, all from the armed WP handler's ChildImport) and binds
// the backend unix socket the child Firecracker's Uffd restore backend connects to
// (passed as mem_backend.backend_path on /snapshot/load). It MUST be called before
// the child /snapshot/load, because Firecracker connects to the socket during the
// load. The returned handler is ready for Receive.
func StartChildUFFDHandler(sockPath string, imp ChildMemfdImport) (ChildUFFDHandle, error) {
	if sockPath == "" {
		return nil, fmt.Errorf("childuffd: empty backend socket path")
	}
	if err := imp.validate(); err != nil {
		return nil, fmt.Errorf("childuffd: invalid import: %w", err)
	}
	pageSize := imp.PageSize
	bmBytes := frozenBitmapBytes(imp.Bytes, pageSize)
	if bmBytes == 0 {
		return nil, fmt.Errorf("childuffd: zero frozen bitmap size")
	}

	// Reopen + identity-verify the three descriptors the parent exported, the same
	// mechanism (and the same fail-closed PID-reuse guarantees) the eager
	// ComposeChildFromImport uses. The parent guest memfd is created by Firecracker,
	// so require only that the reopened target is a memfd (empty want name) and its
	// inode identity matches; FROZEN and the bitmap are the handler's own named memfds.
	parentFd, closeParent, err := openProcFdVerified(imp.ParentPID, imp.ParentFD, "", imp.ParentIno, imp.ParentDev)
	if err != nil {
		return nil, fmt.Errorf("childuffd: open parent memfd: %w", err)
	}
	defer closeParent()
	frozenFd, closeFrozen, err := openProcFdVerified(imp.FrozenPID, imp.FrozenFD, frozenMemfdName, imp.FrozenIno, imp.FrozenDev)
	if err != nil {
		return nil, fmt.Errorf("childuffd: open frozen memfd: %w", err)
	}
	defer closeFrozen()
	bmFd, closeBM, err := openProcFdVerified(imp.BitmapPID, imp.BitmapFD, frozenBitmapName, imp.BitmapIno, imp.BitmapDev)
	if err != nil {
		return nil, fmt.Errorf("childuffd: open frozen bitmap memfd: %w", err)
	}
	defer closeBM()

	// live is MAP_SHARED read-only so the handler reads the source's CURRENT bytes at
	// fault time: a page the source has not rewritten since the fork still holds its
	// fork-time value, and a page it DID rewrite is served from frozen instead (the
	// frozen bit is set before that write lands). frozen + bm are the fork-time image
	// and its live selector.
	live, err := unix.Mmap(parentFd, 0, int(imp.Bytes), unix.PROT_READ, unix.MAP_SHARED)
	if err != nil {
		return nil, fmt.Errorf("childuffd: mmap parent memfd: %w", err)
	}
	frozen, err := unix.Mmap(frozenFd, 0, int(imp.Bytes), unix.PROT_READ, unix.MAP_SHARED)
	if err != nil {
		_ = unix.Munmap(live)
		return nil, fmt.Errorf("childuffd: mmap frozen memfd: %w", err)
	}
	bm, err := unix.Mmap(bmFd, 0, int(bmBytes), unix.PROT_READ, unix.MAP_SHARED)
	if err != nil {
		_ = unix.Munmap(live)
		_ = unix.Munmap(frozen)
		return nil, fmt.Errorf("childuffd: mmap frozen bitmap memfd: %w", err)
	}
	stage, err := unix.Mmap(-1, 0, int(pageSize), unix.PROT_READ|unix.PROT_WRITE, unix.MAP_PRIVATE|unix.MAP_ANONYMOUS)
	if err != nil {
		_ = unix.Munmap(live)
		_ = unix.Munmap(frozen)
		_ = unix.Munmap(bm)
		return nil, fmt.Errorf("childuffd: mmap stage page: %w", err)
	}

	// Remove a stale socket so the bind does not fail with EADDRINUSE after a crashed
	// prior spawn (matches the WP handler and restore-handler cleanup).
	_ = os.Remove(sockPath)
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: sockPath, Net: "unix"})
	if err != nil {
		_ = unix.Munmap(live)
		_ = unix.Munmap(frozen)
		_ = unix.Munmap(bm)
		_ = unix.Munmap(stage)
		return nil, fmt.Errorf("childuffd: listen %s: %w", sockPath, err)
	}
	return &childUFFDHandler{
		sockPath: sockPath,
		ln:       ln,
		live:     live,
		frozen:   frozen,
		bm:       bm,
		bytes:    imp.Bytes,
		pageSize: pageSize,
		stage:    stage,
		uffd:     -1,
	}, nil
}

// Receive accepts the child Firecracker's connection and reads the single
// GuestRegionUffdMapping handshake message: the region layout (JSON body) and the
// child userfaultfd (SCM_RIGHTS). It reuses the same wire decode the WP handshake
// uses (recvUffdHandshake). Run it concurrently with the child /snapshot/load.
func (h *childUFFDHandler) Receive() error {
	conn, err := h.ln.AcceptUnix()
	if err != nil {
		return fmt.Errorf("childuffd: accept: %w", err)
	}
	defer conn.Close()

	uffdFD, regions, err := recvUffdHandshake(conn)
	if err != nil {
		return err
	}

	// Wrap the uffd in an *os.File so Serve's read runs on the Go runtime poller
	// (Close unblocks it cleanly). The raw h.uffd alias is used only for UFFDIO_COPY,
	// which ignores O_NONBLOCK; the file OWNS the fd so Close never double-closes it.
	uffdFile := os.NewFile(uintptr(uffdFD), "childuffd-uffd")

	h.mu.Lock()
	h.uffd = uffdFD
	h.uffdFile = uffdFile
	h.regions = regions
	h.mu.Unlock()
	return nil
}

// Serve runs the child page-fault loop until Close. On each fault it composes the
// faulting page's fork-time bytes into the stage page and UFFDIO_COPYs them into
// the guest. It returns when the child uffd is closed (Close) or on a fatal error.
func (h *childUFFDHandler) Serve() error {
	h.mu.Lock()
	uffdFile := h.uffdFile
	h.mu.Unlock()
	if uffdFile == nil {
		return fmt.Errorf("childuffd: Serve before handshake")
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
			return fmt.Errorf("childuffd: read uffd: %w", err)
		}
		if nread != int(unsafe.Sizeof(msg)) || msg.Event != uffdEventPagefault {
			// REMOVE/REMAP/UNMAP events need no page fill; ignore them.
			continue
		}
		if err := h.serveFault(msg.PfAddress); err != nil {
			return err
		}
	}
}

// serveFault fills one faulting guest page at host address addr: it composes the
// page's fork-time bytes into the stage page (FROZEN for a frozen page, the live
// source memfd otherwise, with a fault-time re-check that closes the leak window)
// and UFFDIO_COPYs the stage page into the guest.
func (h *childUFFDHandler) serveFault(addr uint64) error {
	h.mu.Lock()
	regions := h.regions
	uffd := h.uffd
	h.mu.Unlock()
	if uffd < 0 {
		return fmt.Errorf("childuffd: serveFault before handshake")
	}
	pageSize := h.pageSize
	pageBase, fileOffset, ok := fileOffsetForAddr(regions, addr, pageSize)
	if !ok {
		// A fault outside the registered range: skip it (defensive).
		return nil
	}
	if fileOffset+pageSize > h.bytes {
		return fmt.Errorf("childuffd: fault page [%#x,+%#x) past guest RAM %#x (addr=%#x)", fileOffset, pageSize, h.bytes, addr)
	}
	if fileOffset+pageSize > uint64(len(h.live)) || fileOffset+pageSize > uint64(len(h.frozen)) {
		return fmt.Errorf("childuffd: fault page [%#x,+%#x) past mapped source views (live=%d frozen=%d)", fileOffset, pageSize, len(h.live), len(h.frozen))
	}
	pageIdx := fileOffset / pageSize

	// PER-FAULT frozen composite (the no-leak invariant). If the page is frozen, its
	// fork-time bytes are in FROZEN, stable forever (the WP handler writes FROZEN then
	// sets the bit, once, before it unprotects and lets the source write land). If it
	// is not frozen, the source has provably not written it since the fork (still
	// write-protected), so the live memfd holds the fork-time value: copy it. Then
	// RE-CHECK the bit: a concurrent freeze between the read above and the copy could
	// have set the bit and let the source's write land into live, so a set bit now
	// means the value we copied might be the source's NEW value; override it with the
	// FROZEN fork-time bytes. Either way the stage ends holding the fork-time value.
	if testFrozenBit(h.bm, pageIdx) {
		copy(h.stage, h.frozen[fileOffset:fileOffset+pageSize])
	} else {
		copy(h.stage, h.live[fileOffset:fileOffset+pageSize])
		if testFrozenBit(h.bm, pageIdx) {
			copy(h.stage, h.frozen[fileOffset:fileOffset+pageSize])
		}
	}

	if err := h.copyPage(pageBase, pageSize); err != nil {
		return err
	}
	atomic.AddInt64(&h.served, 1)
	return nil
}

// copyPage UFFDIO_COPYs pageSize composed bytes from the stage page into the guest
// at dst. An already-present page (EEXIST) is treated as success: it only means the
// page was filled meanwhile, which is the desired end state.
func (h *childUFFDHandler) copyPage(dst, pageSize uint64) error {
	arg := uffdioCopyArg{
		Dst: dst,
		Src: uint64(uintptr(unsafe.Pointer(&h.stage[0]))),
		Len: pageSize,
	}
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(h.uffd), uintptr(uffdioCopy), uintptr(unsafe.Pointer(&arg)))
	if errno != 0 {
		if errno == unix.EEXIST {
			return nil
		}
		return fmt.Errorf("childuffd: UFFDIO_COPY dst=%#x len=%#x: %w", dst, pageSize, errno)
	}
	return nil
}

// FaultCount returns the number of page faults Serve has filled (the child's
// working set). Safe to call concurrently.
func (h *childUFFDHandler) FaultCount() int64 { return atomic.LoadInt64(&h.served) }

// Close tears the handler down: it closes the child uffd (unblocking Serve's
// poller-driven read), the socket, removes the socket path, and munmaps the source
// views and the stage page. Idempotent.
func (h *childUFFDHandler) Close() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return nil
	}
	h.closed = true
	if h.ln != nil {
		_ = h.ln.Close()
	}
	// Closing the file unblocks Serve's poller-driven Read and closes the uffd fd it
	// owns. Clear the raw alias without a second unix.Close (double close).
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
	if h.frozen != nil {
		_ = unix.Munmap(h.frozen)
		h.frozen = nil
	}
	if h.bm != nil {
		_ = unix.Munmap(h.bm)
		h.bm = nil
	}
	if h.stage != nil {
		_ = unix.Munmap(h.stage)
		h.stage = nil
	}
	_ = os.Remove(h.sockPath)
	return nil
}
