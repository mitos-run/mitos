//go:build linux

package fork

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// This is the milestone m4b real-KVM correctness test: it drives the live-cow
// fork WP handler (wpfork_linux.go) end-to-end against a REAL Linux userfaultfd
// write-protect over a REAL memfd, and proves the m2 invariant through the Go
// handler code (not a mock): a co-located child INHERITS the parent's fork-time
// guest memory, and a resumed parent overwriting its memory does NOT leak that
// write into the child. It runs in the firecracker-test job (go test
// ./internal/fork/... on a real KVM/Linux runner).
//
// It stands in for the patched Firecracker's parent side exactly as
// mitos-run/firecracker src/vmm/src/vstate/memory.rs::mitos_wp_offer does: create
// the guest RAM as a MAP_SHARED memfd, publish the m1 export, create a
// userfaultfd requesting UFFD_FEATURE_PAGEFAULT_FLAG_WP, register the region in
// write-protect mode, and hand the uffd + region layout to the handler over a
// unix socket via SCM_RIGHTS. The handler under test then owns the real
// freeze/copy/unprotect loop. The only thing not booted here is a full guest VMM
// (that needs the co-located child memfd-import Firecracker patch + a KVM node,
// the documented follow-up); the memory-sharing correctness mechanism that is the
// heart of m4b is exercised for real.

// userfaultfd + ioctl constants used to STAND IN for the patched Firecracker
// parent side (linux/userfaultfd.h, asm-generic ioctl encoding).
const (
	uffdUserModeOnly = 0x1 // UFFD_USER_MODE_ONLY: our writer is a userspace thread
	// UFFDIO_API = _IOWR(0xAA, 0x3F, struct uffdio_api{24}) = 0xc018aa3f.
	uffdioAPICmd = 0xc018aa3f
	// UFFDIO_REGISTER = _IOWR(0xAA, 0x00, struct uffdio_register{32}) = 0xc020aa00.
	uffdioRegisterCmd     = 0xc020aa00
	uffdAPIVersion        = 0xAA // UFFD_API
	featPagefaultFlagWP   = 0x1  // UFFD_FEATURE_PAGEFAULT_FLAG_WP
	registerModeWP        = 0x2  // UFFDIO_REGISTER_MODE_WP
	uffdioWriteprotectBit = 0x06 // _UFFDIO_WRITEPROTECT (bit in reg.ioctls)

	wpTestMarker = "MITOS-M4B-ORIGINAL-FORK-TIME-PAGE"
	wpTestNewVal = "MITOS-M4B-PARENT-OVERWROTE-THIS!!"
)

type uffdioAPIArg struct{ API, Features, Ioctls uint64 }
type uffdioRegisterArg struct {
	Start, Len, Mode, Ioctls uint64
}

// createWPUffd creates a userfaultfd that requests write-protect faults, exactly
// as the patched Firecracker does. It returns skip=true (with a reason) when the
// kernel or sandbox cannot support UFFD write-protect, which is the m2
// precondition (a WP-capable kernel with CONFIG_HAVE_ARCH_USERFAULTFD_WP), so the
// test is honest on an incapable box rather than falsely failing.
func createWPUffd(t *testing.T) (int, bool, string) {
	t.Helper()
	// Probe fd: UFFDIO_API can only be called once per fd, so query the supported
	// feature mask on a throwaway fd first (mirrors the reference C harness).
	probe, _, errno := unix.Syscall(unix.SYS_USERFAULTFD, uintptr(unix.O_CLOEXEC|unix.O_NONBLOCK|uffdUserModeOnly), 0, 0)
	if errno != 0 {
		return -1, true, fmt.Sprintf("userfaultfd() unavailable (errno %v); needs Linux and CAP_SYS_PTRACE or vm.unprivileged_userfaultfd=1", errno)
	}
	pfd := int(probe)
	q := uffdioAPIArg{API: uffdAPIVersion}
	if _, _, e := unix.Syscall(unix.SYS_IOCTL, uintptr(pfd), uintptr(uffdioAPICmd), uintptr(unsafe.Pointer(&q))); e != 0 {
		_ = unix.Close(pfd)
		return -1, true, fmt.Sprintf("UFFDIO_API query failed (errno %v): kernel too old for userfaultfd", e)
	}
	_ = unix.Close(pfd)
	if q.Features&featPagefaultFlagWP == 0 {
		return -1, true, fmt.Sprintf("kernel lacks UFFD write-protect: UFFD_FEATURE_PAGEFAULT_FLAG_WP (0x%x) absent from supported mask 0x%x", featPagefaultFlagWP, q.Features)
	}

	fd, _, errno := unix.Syscall(unix.SYS_USERFAULTFD, uintptr(unix.O_CLOEXEC|unix.O_NONBLOCK|uffdUserModeOnly), 0, 0)
	if errno != 0 {
		return -1, true, fmt.Sprintf("userfaultfd() (real) errno %v", errno)
	}
	api := uffdioAPIArg{API: uffdAPIVersion, Features: featPagefaultFlagWP}
	if _, _, e := unix.Syscall(unix.SYS_IOCTL, uintptr(int(fd)), uintptr(uffdioAPICmd), uintptr(unsafe.Pointer(&api))); e != 0 {
		_ = unix.Close(int(fd))
		return -1, true, fmt.Sprintf("UFFDIO_API requesting PAGEFAULT_FLAG_WP failed (errno %v)", e)
	}
	return int(fd), false, ""
}

// registerWP registers [addr,addr+len) with the uffd in write-protect mode, as
// the patched Firecracker does over each guest region.
func registerWP(t *testing.T, uffd int, addr, length uint64) bool {
	t.Helper()
	reg := uffdioRegisterArg{Start: addr, Len: length, Mode: registerModeWP}
	if _, _, e := unix.Syscall(unix.SYS_IOCTL, uintptr(uffd), uintptr(uffdioRegisterCmd), uintptr(unsafe.Pointer(&reg))); e != 0 {
		t.Logf("UFFDIO_REGISTER MODE_WP failed (errno %v): kernel lacks WP over this mapping", e)
		return false
	}
	if reg.Ioctls&(1<<uffdioWriteprotectBit) == 0 {
		t.Logf("UFFDIO_WRITEPROTECT not offered for range: kernel WP over memfd unsupported")
		return false
	}
	return true
}

// sendUffd hands the uffd + JSON region layout to the handler over conn via
// SCM_RIGHTS, exactly as the patched Firecracker's ScmSocket::send_with_fd does.
func sendUffd(t *testing.T, conn *net.UnixConn, body []byte, uffd int) {
	t.Helper()
	raw, err := conn.SyscallConn()
	if err != nil {
		t.Fatalf("syscallconn: %v", err)
	}
	rights := unix.UnixRights(uffd)
	var serr error
	if cerr := raw.Write(func(fd uintptr) bool {
		serr = unix.Sendmsg(int(fd), body, rights, nil, 0)
		return true
	}); cerr != nil {
		t.Fatalf("rawconn write: %v", cerr)
	}
	if serr != nil {
		t.Fatalf("sendmsg uffd: %v", serr)
	}
}

// TestLiveCowForkInheritanceAndNoLeak is the m4b correctness gate. It exercises
// the WP handler end-to-end over a real userfaultfd + memfd and asserts BOTH
// halves of the m2 invariant, now driven through the Go handler:
//
//	INHERITANCE: a co-located child reads the parent's fork-time guest memory
//	    (both the marker page and an untouched page).
//	NO LEAK: the resumed parent OVERWRITES the marker page, and the child still
//	    reads the ORIGINAL fork-time bytes, not the parent's post-fork write.
func TestLiveCowForkInheritanceAndNoLeak(t *testing.T) {
	const pageSize = 4096
	const npages = 8
	size := uint64(npages * pageSize)

	// --- Stand in for the patched Firecracker parent: guest RAM as MAP_SHARED
	// memfd, markers written, m1 export published. ---
	guestFd, err := unix.MemfdCreate("mitos-guest-ram", unix.MFD_CLOEXEC)
	if err != nil {
		t.Fatalf("memfd_create guest: %v", err)
	}
	defer unix.Close(guestFd)
	if err := unix.Ftruncate(guestFd, int64(size)); err != nil {
		t.Fatalf("ftruncate guest: %v", err)
	}
	guest, err := unix.Mmap(guestFd, 0, int(size), unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
	if err != nil {
		t.Fatalf("mmap guest: %v", err)
	}
	defer unix.Munmap(guest)

	// Page 0 gets the MARKER (the page the parent will clobber); page 1 a distinct
	// pattern so inheritance of an untouched page is checked too.
	copy(guest[0:], wpTestMarker)
	for off := pageSize; off < int(size); off += pageSize {
		guest[off] = byte((off / pageSize) & 0xff)
	}

	dir := t.TempDir()
	exportPath := filepath.Join(dir, "memfd_export")
	if err := os.WriteFile(exportPath, []byte(fmt.Sprintf("%d %d %d\n", os.Getpid(), guestFd, size)), 0o600); err != nil {
		t.Fatalf("write export: %v", err)
	}
	udsPath := filepath.Join(dir, "wp.sock")

	// --- Arm the handler (binds the UDS) BEFORE the "parent" connects. ---
	handle, err := StartWPForkHandler(WPForkConfig{UDSPath: udsPath, MemExportPath: exportPath})
	if err != nil {
		t.Fatalf("StartWPForkHandler: %v", err)
	}
	defer handle.Close()

	// --- Create + register the WP uffd (the mitos_wp_offer side). ---
	uffd, skip, reason := createWPUffd(t)
	if skip {
		t.Skipf("m2 precondition not met on this runner: %s", reason)
	}
	base := uint64(uintptr(unsafe.Pointer(&guest[0])))
	if !registerWP(t, uffd, base, size) {
		t.Skipf("m2 precondition not met: WP register/offer failed over the memfd mapping")
	}

	// Handler Receive runs concurrently with the "parent" connecting + sending.
	var wg sync.WaitGroup
	wg.Add(1)
	var recvErr error
	go func() {
		defer wg.Done()
		recvErr = handle.Receive()
	}()

	conn, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: udsPath, Net: "unix"})
	if err != nil {
		t.Fatalf("dial WP UDS: %v", err)
	}
	body := []byte(fmt.Sprintf(`[{"base_host_virt_addr":%d,"size":%d,"offset":0,"page_size":%d}]`, base, size, pageSize))
	sendUffd(t, conn, body, uffd)
	_ = conn.Close()
	wg.Wait()
	if recvErr != nil {
		t.Fatalf("handler Receive: %v", recvErr)
	}

	// --- Fork point: FREEZE, then run Serve, then RESUME the parent. ---
	freeze, err := handle.Freeze()
	if err != nil {
		t.Fatalf("Freeze: %v", err)
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- handle.Serve() }()

	// The resumed parent overwrites page 0. The write takes a WP fault and blocks
	// until the handler copies the fork-time bytes into FROZEN and unprotects.
	writeDone := make(chan struct{})
	go func() {
		copy(guest[0:], wpTestNewVal)
		close(writeDone)
	}()
	select {
	case <-writeDone:
	case <-time.After(10 * time.Second):
		t.Fatalf("parent overwrite of page 0 never completed: WP fault not served (freeze took %v, faults=%d)", freeze, handle.FaultCount())
	}

	// Wait until the handler has recorded page 0 as frozen (source selector set).
	deadline := time.Now().Add(5 * time.Second)
	for !handle.FrozenPage(0) {
		if time.Now().After(deadline) {
			t.Fatalf("handler never marked page 0 frozen; faults=%d", handle.FaultCount())
		}
		time.Sleep(time.Millisecond)
	}

	// --- Co-located CHILD: MAP_PRIVATE the guest memfd and the FROZEN memfd,
	// source-select per page (frozen page -> FROZEN, else live), like FC's
	// restore handler. ---
	childLive, err := unix.Mmap(guestFd, 0, int(size), unix.PROT_READ, unix.MAP_PRIVATE)
	if err != nil {
		t.Fatalf("child mmap live: %v", err)
	}
	defer unix.Munmap(childLive)
	frozenFd := handle.FrozenFd()
	if frozenFd < 0 {
		t.Fatalf("handler exposed no frozen fd")
	}
	childFrozen, err := unix.Mmap(frozenFd, 0, int(size), unix.PROT_READ, unix.MAP_PRIVATE)
	if err != nil {
		t.Fatalf("child mmap frozen: %v", err)
	}
	defer unix.Munmap(childFrozen)

	// Page 0 was clobbered -> child reads it from FROZEN and MUST see the ORIGINAL
	// fork-time marker (NO LEAK of the parent's post-fork write).
	if !handle.FrozenPage(0) {
		t.Fatalf("page 0 must be marked frozen")
	}
	got0 := string(childFrozen[0:len(wpTestMarker)])
	if got0 != wpTestMarker {
		t.Errorf("NO-LEAK VIOLATED: child read %q from page 0, want the fork-time marker %q", got0, wpTestMarker)
	}

	// Page 1 was never touched -> child reads it live and MUST see the fork-time
	// pattern (INHERITANCE of an untouched page). The guest fill wrote page index i
	// as byte(i & 0xff), so page 1 holds byte(1).
	const page1Pattern = byte(1)
	if got := childLive[pageSize]; got != page1Pattern {
		t.Errorf("INHERITANCE VIOLATED: child read untouched page 1 byte = %#x, want %#x", got, page1Pattern)
	}

	// The raw live view of page 0 now holds the parent's NEW value. This is what a
	// naive MAP_PRIVATE reader that ignored the frozen source would leak; it proves
	// the frozen source is doing real work, not masking a no-op.
	if got := string(childLive[0:len(wpTestNewVal)]); got != wpTestNewVal {
		t.Errorf("expected raw live page 0 to hold the parent's overwrite %q, got %q", wpTestNewVal, got)
	}

	// The parent's own view sees its overwrite (the write landed after unprotect).
	if got := string(guest[0:len(wpTestNewVal)]); got != wpTestNewVal {
		t.Errorf("parent view of page 0 = %q, want its overwrite %q", got, wpTestNewVal)
	}

	if err := handle.Close(); err != nil {
		t.Fatalf("handler Close: %v", err)
	}
	select {
	case err := <-serveErr:
		if err != nil {
			t.Errorf("Serve returned error after Close: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Errorf("Serve did not return after Close")
	}

	t.Logf("m4b live-cow correctness PASS: inheritance + no-leak; freeze=%v, WP faults served=%d", freeze, handle.FaultCount())
}
