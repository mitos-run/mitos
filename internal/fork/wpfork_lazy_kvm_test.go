//go:build linux

package fork

import (
	"encoding/json"
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

const (
	// featMissingShmem is UFFD_FEATURE_MISSING_SHMEM (1<<5). Without negotiating it
	// the kernel NEVER delivers MISSING faults for a shmem (memfd) VMA: the read is
	// silently satisfied by the zero page. That would leave a lazily restored guest
	// running on zeroed RAM with no fault to tell anyone, so the lazy restore is
	// only ever armed when this feature is granted.
	featMissingShmem = 0x20
	// uffdioCopyBit is _UFFDIO_COPY, the bit the kernel sets in reg.ioctls when the
	// range supports UFFDIO_COPY (how the handler installs a faulting page).
	// registerModeMissing (UFFDIO_REGISTER_MODE_MISSING) comes from childuffd_linux_test.go.
	uffdioCopyBit = 0x03
)

// createLazyUffd builds a userfaultfd that can serve BOTH the lazy restore's MISSING
// faults and the live-cow freezer's WP faults over one shmem mapping, which is the
// registration the patched Firecracker performs on a lazily restored source. The
// kernel permits exactly one uffd per VMA, so MISSING and WP must share it.
func createLazyUffd(t *testing.T) (int, bool, string) {
	t.Helper()
	probe, _, errno := unix.Syscall(unix.SYS_USERFAULTFD, uintptr(unix.O_CLOEXEC|unix.O_NONBLOCK|uffdUserModeOnly), 0, 0)
	if errno != 0 {
		return -1, true, fmt.Sprintf("userfaultfd() unavailable (errno %v)", errno)
	}
	pfd := int(probe)
	q := uffdioAPIArg{API: uffdAPIVersion}
	if _, _, e := unix.Syscall(unix.SYS_IOCTL, uintptr(pfd), uintptr(uffdioAPICmd), uintptr(unsafe.Pointer(&q))); e != 0 {
		_ = unix.Close(pfd)
		return -1, true, fmt.Sprintf("UFFDIO_API query failed (errno %v)", e)
	}
	_ = unix.Close(pfd)
	if q.Features&featPagefaultFlagWP == 0 || q.Features&featMissingShmem == 0 {
		return -1, true, fmt.Sprintf("kernel lacks PAGEFAULT_FLAG_WP|MISSING_SHMEM (mask 0x%x)", q.Features)
	}

	fd, _, errno := unix.Syscall(unix.SYS_USERFAULTFD, uintptr(unix.O_CLOEXEC|unix.O_NONBLOCK|uffdUserModeOnly), 0, 0)
	if errno != 0 {
		return -1, true, fmt.Sprintf("userfaultfd() (real) errno %v", errno)
	}
	api := uffdioAPIArg{API: uffdAPIVersion, Features: featPagefaultFlagWP | featMissingShmem}
	if _, _, e := unix.Syscall(unix.SYS_IOCTL, uintptr(int(fd)), uintptr(uffdioAPICmd), uintptr(unsafe.Pointer(&api))); e != 0 {
		_ = unix.Close(int(fd))
		return -1, true, fmt.Sprintf("UFFDIO_API requesting WP|MISSING_SHMEM failed (errno %v)", e)
	}
	return int(fd), false, ""
}

func registerMissingWP(t *testing.T, uffd int, addr, length uint64) bool {
	t.Helper()
	reg := uffdioRegisterArg{Start: addr, Len: length, Mode: registerModeMissing | registerModeWP}
	if _, _, e := unix.Syscall(unix.SYS_IOCTL, uintptr(uffd), uintptr(uffdioRegisterCmd), uintptr(unsafe.Pointer(&reg))); e != 0 {
		t.Logf("UFFDIO_REGISTER MISSING|WP failed (errno %v)", e)
		return false
	}
	if reg.Ioctls&(1<<uffdioCopyBit) == 0 || reg.Ioctls&(1<<uffdioWriteprotectBit) == 0 {
		t.Logf("range offers ioctls 0x%x; need UFFDIO_COPY and UFFDIO_WRITEPROTECT", reg.Ioctls)
		return false
	}
	return true
}

// TestLiveCowLazyRestoreServesMissingAndFillsResidualOnFreeze is the correctness gate
// for the LAZY live-cow restore, driven over a real userfaultfd + memfd.
//
// The lazy restore replaces the eager O(guest RAM) copy that ran inside
// PUT /snapshot/load (~195 ms of a 218 ms warm-claim activate on a 512 MiB guest)
// with an EMPTY shared memfd whose pages arrive on MISSING faults. Two properties
// have to hold or a guest runs on wrong memory:
//
//	POPULATION: a MISSING fault installs the snapshot's bytes for that chunk, so the
//	    guest reads its snapshot RAM and not the zero page.
//	RESIDUAL-ON-FREEZE: a co-located fork child maps the parent's memfd MAP_PRIVATE,
//	    so any chunk the parent never faulted in would be a HOLE the child reads as
//	    ZEROS. Freeze (the fork point) must fill every unpopulated chunk first, so the
//	    child sees the whole snapshot. Filling there, rather than at restore, is what
//	    keeps warm-claim activate O(1) and charges the fill to forks only.
//
// It also asserts the pre-existing m2 invariant still holds on the lazy path: a
// post-freeze parent write is captured at its fork-time value.
func TestLiveCowLazyRestoreServesMissingAndFillsResidualOnFreeze(t *testing.T) {
	requireKVMCIRunner(t)

	// Two full chunks plus a short tail, so residual-fill has real work after only
	// the first chunk is demand-faulted, and the tail exercises the region clip.
	const pageSize = 4096
	size := uint64(2*lazyChunkBytes + pageSize)

	// --- The snapshot mem file: every page carries a distinct, non-zero pattern, so
	// a page served from the zero page (or from the wrong offset) is unmistakable. ---
	dir := t.TempDir()
	memPath := filepath.Join(dir, "mem")
	want := make([]byte, size)
	for off := uint64(0); off < size; off += pageSize {
		fill := byte((off/pageSize)%251 + 1) // never 0
		for i := off; i < off+pageSize && i < size; i++ {
			want[i] = fill
		}
	}
	if err := os.WriteFile(memPath, want, 0o600); err != nil {
		t.Fatalf("write mem file: %v", err)
	}

	// --- Stand in for the patched Firecracker: guest RAM is an EMPTY MAP_SHARED
	// memfd (snapshot_memfd_lazy), not a copy of the mem file. ---
	guestFd, err := unix.MemfdCreate("mitos-guest-ram-lazy", unix.MFD_CLOEXEC)
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

	exportPath := filepath.Join(dir, "memfd_export")
	if err := os.WriteFile(exportPath, []byte(fmt.Sprintf("%d %d %d\n", os.Getpid(), guestFd, size)), 0o600); err != nil {
		t.Fatalf("write export: %v", err)
	}
	udsPath := filepath.Join(dir, "wp.sock")

	handle, err := StartWPForkHandler(WPForkConfig{UDSPath: udsPath, MemExportPath: exportPath})
	if err != nil {
		t.Fatalf("StartWPForkHandler: %v", err)
	}
	defer handle.Close()

	// The husk does this before the source loads the snapshot; without it the handler
	// cannot serve a MISSING fault and the guest would hang.
	if err := handle.SetMemSource(memPath); err != nil {
		t.Fatalf("SetMemSource: %v", err)
	}

	uffd, skip, reason := createLazyUffd(t)
	if skip {
		t.Skipf("lazy-restore precondition not met on this runner: %s", reason)
	}
	// sendUffd hands the handler a DUPLICATE via SCM_RIGHTS; this descriptor stays
	// ours, so close it or the KVM suite leaks one fd per run.
	defer unix.Close(uffd)
	base := uint64(uintptr(unsafe.Pointer(&guest[0])))
	if !registerMissingWP(t, uffd, base, size) {
		t.Skip("lazy-restore precondition not met: MISSING|WP register failed over the memfd mapping")
	}

	body, err := json.Marshal([]uffdMapping{{
		BaseHostVirtAddr: base, Size: size, Offset: 0, PageSize: pageSize,
	}})
	if err != nil {
		t.Fatalf("marshal mappings: %v", err)
	}

	var wg sync.WaitGroup
	wg.Add(1)
	var recvErr error
	go func() {
		defer wg.Done()
		recvErr = handle.Receive()
	}()
	conn, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: udsPath, Net: "unix"})
	if err != nil {
		t.Fatalf("dial wp uds: %v", err)
	}
	sendUffd(t, conn, body, uffd)
	wg.Wait()
	if recvErr != nil {
		t.Fatalf("Receive: %v", recvErr)
	}
	go func() { _ = handle.Serve() }()

	// --- POPULATION: touch ONE page. It must fault MISSING and come back with the
	// mem file's bytes for the whole surrounding chunk. ---
	if got := guest[pageSize]; got != want[pageSize] {
		t.Fatalf("demand-faulted page = %#x, want %#x (served from the zero page?)", got, want[pageSize])
	}
	for i := uint64(0); i < lazyChunkBytes; i += pageSize {
		if guest[i] != want[i] {
			t.Fatalf("chunk 0 offset %#x = %#x, want %#x", i, guest[i], want[i])
		}
	}
	lazyHandle, ok := handle.(*wpForkHandler)
	if !ok {
		t.Fatalf("handle is %T, want *wpForkHandler", handle)
	}
	if n := lazyHandle.MissingFaultCount(); n != 1 {
		t.Fatalf("MissingFaultCount = %d after touching one chunk, want exactly 1 (2 MiB chunking)", n)
	}

	// --- RESIDUAL-ON-FREEZE: chunks 1 and 2 were never touched. Freeze must fill them
	// from the mem file, or a co-located child MAP_PRIVATEing this memfd reads zeros. ---
	if _, err := handle.Freeze(); err != nil {
		t.Fatalf("Freeze: %v", err)
	}
	// Read through a SECOND, uffd-UNregistered mapping of the same memfd: this is
	// exactly what the child does (it maps the parent's memfd), so it proves the child's
	// view is complete rather than proving our own faults get served.
	childView, err := unix.Mmap(guestFd, 0, int(size), unix.PROT_READ, unix.MAP_PRIVATE)
	if err != nil {
		t.Fatalf("mmap child view: %v", err)
	}
	defer unix.Munmap(childView)
	for off := uint64(0); off < size; off += pageSize {
		if childView[off] != want[off] {
			t.Fatalf("child view offset %#x = %#x, want %#x: residual chunk was NOT filled at the fork point",
				off, childView[off], want[off])
		}
	}
	if n := lazyHandle.MissingFaultCount(); n != 1 {
		t.Errorf("MissingFaultCount = %d; residual fill must not go through MISSING faults", n)
	}

	// --- m2 invariant still holds on the lazy path: a post-freeze parent write is
	// captured at its fork-time value in the frozen epoch. ---
	origPage0 := want[0]
	guest[0] = 0xEE
	deadline := time.Now().Add(5 * time.Second)
	for !lazyHandle.FrozenPage(0) && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if !lazyHandle.FrozenPage(0) {
		t.Fatal("parent wrote page 0 after the freeze but the handler never captured it")
	}
	frozen, err := unix.Mmap(lazyHandle.FrozenFd(), 0, int(size), unix.PROT_READ, unix.MAP_PRIVATE)
	if err != nil {
		t.Fatalf("mmap frozen: %v", err)
	}
	defer unix.Munmap(frozen)
	if frozen[0] != origPage0 {
		t.Errorf("frozen page 0 = %#x, want the fork-time byte %#x (parent's post-fork write leaked)", frozen[0], origPage0)
	}
}
