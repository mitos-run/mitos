//go:build linux

package fork

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"unsafe"

	"golang.org/x/sys/unix"

	"mitos.run/mitos/internal/cas"
)

// This file is the Linux userfaultfd (UFFD) memory backend for Firecracker
// snapshot restore (issue #167). It is the mechanism that makes hugepage-backed
// restore possible at all (Firecracker refuses to file-map a hugetlbfs snapshot)
// and that pays the lazy-fault tail up front by PRELOADING a captured hot-page
// set before the guest resumes.
//
// Protocol (Firecracker GuestRegionUffdMapping handshake): the handler binds a
// unix socket and the engine points /snapshot/load at it via
// mem_backend.backend_path. During load Firecracker connects, creates a
// userfaultfd over the guest memory, and sends ONE message: a JSON array of
// region mappings as the body and the userfaultfd file descriptor as SCM_RIGHTS
// ancillary data. The handler then services UFFD_EVENT_PAGEFAULT events with
// UFFDIO_COPY, sourcing page bytes from the snapshot mem file (mmap'd read-only).
//
// SECURITY (threat-model: restore path). The handler reads ONLY the snapshot mem
// file the restore already trusts (it is part of the content-addressed,
// verify-on-load manifest) and writes ONLY into the guest memory Firecracker
// registered. It introduces no new external input beyond the mappings Firecracker
// itself sends over a private per-VM socket, and no host-path write. The uffd is
// created by Firecracker; the handler never registers ranges or changes guest
// visibility, it only fills missing pages with bytes from the verified snapshot.

// UFFD ioctl and event constants (linux/userfaultfd.h, x86_64). x/sys/unix
// v0.45 ships no UFFD wrappers, so we define exactly what we use.
//
//	UFFDIO_COPY = _IOWR(0xAA, 0x03, struct uffdio_copy) with sizeof==40:
//	  (dir=3<<30) | (size=40<<16) | (type=0xAA<<8) | (nr=0x03) = 0xc028aa03
const (
	uffdioCopy         = 0xc028aa03
	uffdEventPagefault = 0x12
)

// uffdioCopyArg mirrors struct uffdio_copy. Copy is the kernel's output: bytes
// copied, or a negative errno.
type uffdioCopyArg struct {
	Dst  uint64
	Src  uint64
	Len  uint64
	Mode uint64
	Copy int64
}

// uffdMsg mirrors struct uffd_msg (32 bytes). We read the event tag and, for a
// pagefault, the faulting address at offset 16.
type uffdMsg struct {
	Event     uint8
	Reserved1 uint8
	Reserved2 uint16
	Reserved3 uint32
	PfFlags   uint64
	PfAddress uint64
	PfFeat    uint64
}

// uffdHandler owns the UFFD backend for one restored VM: the mmap'd snapshot mem
// file (page source), the unix socket Firecracker connects to, and, once the
// handshake completes, the userfaultfd and region mappings. Lifecycle:
// newUFFDHandler -> receive (during /snapshot/load) -> optional Preload (before
// resume) -> Serve (goroutine, life of the VM) -> Close.
type uffdHandler struct {
	sockPath string
	ln       *net.UnixListener
	memMap   []byte
	memSize  uint64
	capture  bool

	mu      sync.Mutex
	regions []uffdMapping
	uffd    int
	trace   []FaultRecord
	closed  bool

	// served counts the page faults Serve has serviced lazily (NOT the pages
	// Preload filled up front). It is the per-resume runtime fault count the
	// prefetch benchmark reports: preloading converts would-be faults into upfront
	// copies, so the ON arm services fewer here than the OFF arm.
	served int64
}

// FaultCount returns the number of page faults Serve has serviced so far (the
// runtime lazy-fault count for this resume). Safe to call concurrently.
func (h *uffdHandler) FaultCount() int64 {
	return atomic.LoadInt64(&h.served)
}

// newUFFDHandler opens and read-only mmaps the snapshot mem file and binds the
// backend unix socket. The caller passes sockPath as mem_backend.backend_path to
// /snapshot/load. capture=true makes Serve record every serviced fault for
// hot-page capture.
func newUFFDHandler(sockPath, memPath string, capture bool) (*uffdHandler, error) {
	f, err := os.Open(memPath)
	if err != nil {
		return nil, fmt.Errorf("uffd: open mem file %s: %w", memPath, err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("uffd: stat mem file: %w", err)
	}
	size := st.Size()
	if size <= 0 {
		return nil, fmt.Errorf("uffd: mem file %s is empty", memPath)
	}
	mm, err := unix.Mmap(int(f.Fd()), 0, int(size), unix.PROT_READ, unix.MAP_PRIVATE)
	if err != nil {
		return nil, fmt.Errorf("uffd: mmap mem file: %w", err)
	}
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: sockPath, Net: "unix"})
	if err != nil {
		_ = unix.Munmap(mm)
		return nil, fmt.Errorf("uffd: listen %s: %w", sockPath, err)
	}
	return &uffdHandler{sockPath: sockPath, ln: ln, memMap: mm, memSize: uint64(size), capture: capture, uffd: -1}, nil
}

// receive accepts Firecracker's connection and reads the single handshake
// message: the region mappings (JSON body) and the userfaultfd (SCM_RIGHTS). It
// must be called concurrently with the engine's /snapshot/load (which is what
// drives Firecracker to connect). After it returns the handler holds the uffd and
// the region table.
func (h *uffdHandler) receive() error {
	conn, err := h.ln.AcceptUnix()
	if err != nil {
		return fmt.Errorf("uffd: accept: %w", err)
	}
	defer conn.Close()

	raw, err := conn.SyscallConn()
	if err != nil {
		return fmt.Errorf("uffd: syscallconn: %w", err)
	}
	buf := make([]byte, 1<<16)
	oob := make([]byte, 1024)
	var n, oobn int
	var rerr error
	if cerr := raw.Read(func(fd uintptr) bool {
		n, oobn, _, _, rerr = unix.Recvmsg(int(fd), buf, oob, 0)
		return true
	}); cerr != nil {
		return fmt.Errorf("uffd: rawconn read: %w", cerr)
	}
	if rerr != nil {
		return fmt.Errorf("uffd: recvmsg: %w", rerr)
	}

	scms, err := unix.ParseSocketControlMessage(oob[:oobn])
	if err != nil {
		return fmt.Errorf("uffd: parse control message: %w", err)
	}
	var gotFD int = -1
	for _, scm := range scms {
		fds, err := unix.ParseUnixRights(&scm)
		if err != nil {
			continue
		}
		if len(fds) > 0 {
			gotFD = fds[0]
		}
	}
	if gotFD < 0 {
		return fmt.Errorf("uffd: handshake carried no userfaultfd descriptor")
	}

	var regions []uffdMapping
	if err := json.Unmarshal(buf[:n], &regions); err != nil {
		_ = unix.Close(gotFD)
		return fmt.Errorf("uffd: decode region mappings %q: %w", string(buf[:n]), err)
	}
	if len(regions) == 0 {
		_ = unix.Close(gotFD)
		return fmt.Errorf("uffd: handshake carried no region mappings")
	}
	// Serve with blocking reads; closing the fd in Close unblocks the loop.
	_ = unix.SetNonblock(gotFD, false)

	h.mu.Lock()
	h.regions = regions
	h.uffd = gotFD
	h.mu.Unlock()
	return nil
}

// copyPage services one page: it copies pageSize bytes from the mem file at
// fileOffset into the guest at dst via UFFDIO_COPY. An already-present page
// (EEXIST) is treated as success: it only means the page was filled meanwhile
// (e.g. preloaded), which is exactly the desired end state.
func (h *uffdHandler) copyPage(dst, fileOffset, pageSize uint64) error {
	if fileOffset+pageSize > h.memSize {
		return fmt.Errorf("uffd: page [%#x,+%#x) past mem file end %#x", fileOffset, pageSize, h.memSize)
	}
	arg := uffdioCopyArg{
		Dst: dst,
		Src: uint64(uintptr(unsafe.Pointer(&h.memMap[fileOffset]))),
		Len: pageSize,
	}
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(h.uffd), uintptr(uffdioCopy), uintptr(unsafe.Pointer(&arg)))
	if errno != 0 {
		if errno == unix.EEXIST {
			return nil
		}
		return fmt.Errorf("uffd: UFFDIO_COPY dst=%#x len=%#x: %w", dst, pageSize, errno)
	}
	return nil
}

// regionPageSize returns the backing page size in bytes for the region covering
// addr, or 0 if none. Base-page regions report 4 KiB, hugepage regions 2 MiB.
func (h *uffdHandler) regionPageSize(addr uint64) uint64 {
	for _, r := range h.regions {
		if r.containsAddr(addr) {
			return r.pageSizeBytes()
		}
	}
	return 0
}

// Preload faults in a captured hot-page set before the VM resumes, paying the
// lazy-fault tail up front in ascending mem-file order (the sequential order
// SelectHotPages emits). Offsets outside the restored regions are skipped. It
// returns the number of pages actually copied. Must be called after receive and
// before Resume.
func (h *uffdHandler) Preload(set cas.HotPageSet) (int, error) {
	h.mu.Lock()
	regions := h.regions
	h.mu.Unlock()
	if h.uffd < 0 {
		return 0, fmt.Errorf("uffd: Preload before handshake")
	}
	pageSize := uint64(set.PageSizeBytes)
	if pageSize == 0 {
		return 0, fmt.Errorf("uffd: Preload with zero page size")
	}
	copied := 0
	for _, off := range set.Offsets {
		fileOffset := uint64(off)
		hostAddr, ok := hostAddrForFileOffset(regions, fileOffset)
		if !ok {
			continue
		}
		if err := h.copyPage(hostAddr, fileOffset, pageSize); err != nil {
			return copied, err
		}
		copied++
	}
	return copied, nil
}

// Serve runs the UFFD event loop until Close: it reads fault events and fills
// each faulting page from the mem file. In capture mode it also records each
// serviced fault's mem-file offset, which CaptureTrace returns for SelectHotPages.
// It returns when the uffd is closed (Close) or on a fatal read error.
func (h *uffdHandler) Serve() error {
	if h.uffd < 0 {
		return fmt.Errorf("uffd: Serve before handshake")
	}
	var msg uffdMsg
	msgBuf := (*[unsafe.Sizeof(msg)]byte)(unsafe.Pointer(&msg))[:]
	for {
		n, err := unix.Read(h.uffd, msgBuf)
		if err != nil {
			if err == unix.EINTR {
				continue
			}
			h.mu.Lock()
			closed := h.closed
			h.mu.Unlock()
			if closed || err == unix.EBADF {
				return nil
			}
			return fmt.Errorf("uffd: read events: %w", err)
		}
		if n < int(unsafe.Sizeof(msg)) {
			continue
		}
		if msg.Event != uffdEventPagefault {
			// REMOVE/REMAP/UNMAP events need no page fill; ignore them.
			continue
		}
		h.mu.Lock()
		regions := h.regions
		h.mu.Unlock()
		pageSize := h.regionPageSize(msg.PfAddress)
		if pageSize == 0 {
			continue
		}
		pageBase, fileOffset, ok := fileOffsetForAddr(regions, msg.PfAddress, pageSize)
		if !ok {
			continue
		}
		if err := h.copyPage(pageBase, fileOffset, pageSize); err != nil {
			return err
		}
		atomic.AddInt64(&h.served, 1)
		if h.capture {
			h.mu.Lock()
			h.trace = append(h.trace, FaultRecord{Offset: int64(fileOffset)})
			h.mu.Unlock()
		}
	}
}

// CaptureTrace returns the faults serviced so far (capture mode), for
// SelectHotPages to reduce to a manifest hot-page set.
func (h *uffdHandler) CaptureTrace() []FaultRecord {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]FaultRecord, len(h.trace))
	copy(out, h.trace)
	return out
}

// Close tears down the handler: it closes the uffd (unblocking Serve), the
// socket, removes the socket path, and unmaps the mem file.
func (h *uffdHandler) Close() error {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return nil
	}
	h.closed = true
	uffd := h.uffd
	mm := h.memMap
	h.memMap = nil
	h.mu.Unlock()

	if uffd >= 0 {
		_ = unix.Close(uffd)
	}
	if h.ln != nil {
		_ = h.ln.Close()
	}
	_ = os.Remove(h.sockPath)
	if mm != nil {
		_ = unix.Munmap(mm)
	}
	return nil
}
