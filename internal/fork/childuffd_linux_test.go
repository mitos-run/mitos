//go:build linux

package fork

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// This test drives the lazy child-side UFFD handler (childuffd_linux.go) end to end
// over a REAL Linux userfaultfd in MISSING mode plus hand-built memfds, WITHOUT a
// KVM guest, exactly as wpfork_kvm_test.go stands in for the patched Firecracker
// PARENT side. It plays the role of the child Firecracker's stock Uffd restore
// backend: create anonymous "guest RAM", register it MISSING with a userfaultfd,
// hand the uffd + region layout to the handler over the backend socket, then fault
// pages and assert the handler filled each faulting page from the composed source.
// It proves the per-fault frozen composite (childuffd.go): a page whose frozen bit
// is set is served from the FROZEN memfd at its fork-time value EVEN AFTER the live
// source memfd is overwritten (the no-leak invariant), and a page whose bit is
// clear is served from the live source memfd.
//
// It skips (never falsely fails) when userfaultfd is unavailable on the runner
// (needs CAP_SYS_PTRACE or vm.unprivileged_userfaultfd=1), the same honest gating
// createWPUffd uses. The firecracker-test job runs it for real without -race.

// registerModeMissing is UFFDIO_REGISTER_MODE_MISSING: the stock Firecracker Uffd
// backend registers guest RAM in MISSING mode so a first touch of an unfilled page
// faults and the handler UFFDIO_COPYs it in.
const registerModeMissing = 0x1

// createMissingUffd creates a plain userfaultfd (no WP feature) as the stock
// Firecracker Uffd restore backend does. It returns skip=true with a reason when
// userfaultfd is unavailable on this runner.
func createMissingUffd(t *testing.T) (int, bool, string) {
	t.Helper()
	fd, _, errno := unix.Syscall(unix.SYS_USERFAULTFD, uintptr(unix.O_CLOEXEC|unix.O_NONBLOCK|uffdUserModeOnly), 0, 0)
	if errno != 0 {
		return -1, true, fmt.Sprintf("userfaultfd() unavailable (errno %v); needs Linux and CAP_SYS_PTRACE or vm.unprivileged_userfaultfd=1", errno)
	}
	api := uffdioAPIArg{API: uffdAPIVersion}
	if _, _, e := unix.Syscall(unix.SYS_IOCTL, uintptr(int(fd)), uintptr(uffdioAPICmd), uintptr(unsafe.Pointer(&api))); e != 0 {
		_ = unix.Close(int(fd))
		return -1, true, fmt.Sprintf("UFFDIO_API failed (errno %v): kernel too old for userfaultfd", e)
	}
	return int(fd), false, ""
}

// registerMissing registers [addr,addr+len) with the uffd in MISSING mode, as the
// stock Firecracker Uffd backend does over each guest region.
func registerMissing(t *testing.T, uffd int, addr, length uint64) bool {
	t.Helper()
	reg := uffdioRegisterArg{Start: addr, Len: length, Mode: registerModeMissing}
	if _, _, e := unix.Syscall(unix.SYS_IOCTL, uintptr(uffd), uintptr(uffdioRegisterCmd), uintptr(unsafe.Pointer(&reg))); e != 0 {
		t.Logf("UFFDIO_REGISTER MODE_MISSING failed (errno %v)", e)
		return false
	}
	return true
}

// mkNamedMemfd creates a memfd with the given name sized to bytes, returns its fd
// and its (ino, dev) identity. The caller owns the fd.
func mkNamedMemfd(t *testing.T, name string, bytes uint64) (int, uint64, uint64) {
	t.Helper()
	fd, err := unix.MemfdCreate(name, unix.MFD_CLOEXEC)
	if err != nil {
		t.Fatalf("memfd_create %s: %v", name, err)
	}
	if err := unix.Ftruncate(fd, int64(bytes)); err != nil {
		t.Fatalf("ftruncate %s: %v", name, err)
	}
	ino, dev, err := fdIdentity(fd)
	if err != nil {
		t.Fatalf("fdIdentity %s: %v", name, err)
	}
	return fd, ino, dev
}

func TestChildUFFDLazyImportComposesAndNoLeak(t *testing.T) {
	if raceDetectorEnabled {
		t.Skip("skipping child-uffd handshake test under -race (timing-sensitive; the firecracker-test job runs it without -race)")
	}
	const pageSize = 4096
	const npages = 8
	size := uint64(npages * pageSize)

	// Page roles: page frozenPage is a page the parent overwrote after the fork (bit
	// set, fork-time bytes preserved in FROZEN); page livePage is untouched (bit
	// clear, served from the live source memfd). first byte per page carries a
	// distinct tag so the composed source is unambiguous.
	const frozenPage = 2
	const livePage = 5
	const forkTimeTag = byte('F')
	const leakTag = byte('X') // the source's post-fork overwrite that must NOT leak
	const liveTag = byte('N')

	// --- Source (parent) shared memfd: the child faults its guest RAM from here. ---
	parentFd, parentIno, parentDev := mkNamedMemfd(t, "mitos-guest-ram", size)
	defer unix.Close(parentFd) //nolint:errcheck // test teardown
	parent, err := unix.Mmap(parentFd, 0, int(size), unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
	if err != nil {
		t.Fatalf("mmap parent: %v", err)
	}
	defer unix.Munmap(parent) //nolint:errcheck // test teardown
	// Every page starts at its fork-time value.
	parent[livePage*pageSize] = liveTag
	parent[frozenPage*pageSize] = forkTimeTag

	// --- FROZEN memfd + bitmap (the WP handler's per-fork epoch). frozenPage is
	// marked frozen and its fork-time bytes preserved; then the LIVE source page is
	// overwritten with the leak tag, simulating a resumed source's post-fork write. ---
	frozenFd, frozenIno, frozenDev := mkNamedMemfd(t, frozenMemfdName, size)
	defer unix.Close(frozenFd) //nolint:errcheck // test teardown
	frozen, err := unix.Mmap(frozenFd, 0, int(size), unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
	if err != nil {
		t.Fatalf("mmap frozen: %v", err)
	}
	defer unix.Munmap(frozen)                 //nolint:errcheck // test teardown
	frozen[frozenPage*pageSize] = forkTimeTag // fork-time bytes preserved by the WP handler

	bmBytes := frozenBitmapBytes(size, pageSize)
	bmFd, bmIno, bmDev := mkNamedMemfd(t, frozenBitmapName, bmBytes)
	defer unix.Close(bmFd) //nolint:errcheck // test teardown
	bm, err := unix.Mmap(bmFd, 0, int(bmBytes), unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
	if err != nil {
		t.Fatalf("mmap bitmap: %v", err)
	}
	defer unix.Munmap(bm) //nolint:errcheck // test teardown
	setFrozenBit(bm, frozenPage)
	// The resumed source overwrites the frozen page's LIVE bytes: a leak would serve
	// this. The frozen bit is already set, so the handler must serve FROZEN instead.
	parent[frozenPage*pageSize] = leakTag

	imp := ChildMemfdImport{
		ParentPID: os.Getpid(), ParentFD: parentFd, ParentIno: parentIno, ParentDev: parentDev,
		Bytes:     size,
		FrozenPID: os.Getpid(), FrozenFD: frozenFd, FrozenIno: frozenIno, FrozenDev: frozenDev,
		BitmapPID: os.Getpid(), BitmapFD: bmFd, BitmapIno: bmIno, BitmapDev: bmDev,
		PageSize: pageSize,
	}

	sockPath := filepath.Join(t.TempDir(), "cu.sock")
	handleIface, err := StartChildUFFDHandler(sockPath, imp)
	if err != nil {
		t.Fatalf("StartChildUFFDHandler: %v", err)
	}
	h := handleIface.(*childUFFDHandler)
	defer h.Close() //nolint:errcheck // test teardown

	// --- Play the child Firecracker Uffd backend: anonymous guest RAM registered
	// MISSING, uffd + region layout handed to the handler over the socket. ---
	guest, err := unix.Mmap(-1, 0, int(size), unix.PROT_READ|unix.PROT_WRITE, unix.MAP_PRIVATE|unix.MAP_ANONYMOUS)
	if err != nil {
		t.Fatalf("mmap guest: %v", err)
	}
	defer unix.Munmap(guest) //nolint:errcheck // test teardown
	uffd, skip, reason := createMissingUffd(t)
	if skip {
		t.Skipf("userfaultfd precondition not met on this runner: %s", reason)
	}
	base := uint64(uintptr(unsafe.Pointer(&guest[0])))
	if !registerMissing(t, uffd, base, size) {
		t.Skipf("could not register the guest mapping MISSING with userfaultfd on this runner")
	}

	// Receive runs concurrently with the "child" connect+send (Firecracker connects
	// during /snapshot/load).
	recvErr := make(chan error, 1)
	go func() { recvErr <- h.Receive() }()

	conn, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: sockPath, Net: "unix"})
	if err != nil {
		t.Fatalf("dial handler socket: %v", err)
	}
	regions := []uffdMapping{{BaseHostVirtAddr: base, Size: size, Offset: 0, PageSize: pageSize}}
	body, err := json.Marshal(regions)
	if err != nil {
		t.Fatalf("marshal regions: %v", err)
	}
	sendUffd(t, conn, body, uffd)
	_ = conn.Close()

	select {
	case err := <-recvErr:
		if err != nil {
			t.Fatalf("handler Receive: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("handler Receive timed out")
	}

	// Capture the Serve error: if Serve exits (e.g. a compose bug returns an error) a
	// page read would fault forever with no one to fill it, hanging the test to the
	// global timeout. readByte therefore RACES the page load against the Serve error
	// and a short deadline, so a Serve failure or a stuck fill fails fast.
	serveErr := make(chan error, 1)
	go func() { serveErr <- h.Serve() }()

	// Fault a page and return its first byte. Reading it triggers a MISSING fault the
	// handler fills from the composed source; the read runs in its own goroutine so the
	// select can bail if Serve died or the fill wedges.
	readByte := func(page int) byte {
		t.Helper()
		off := page * pageSize
		got := make(chan byte, 1)
		go func() {
			// A volatile-ish read: copy out through a slice so the compiler cannot elide
			// the load that triggers the fault.
			var out [1]byte
			copy(out[:], guest[off:off+1])
			got <- out[0]
		}()
		select {
		case b := <-got:
			return b
		case err := <-serveErr:
			t.Fatalf("handler Serve exited before page %d was filled: %v", page, err)
		case <-time.After(10 * time.Second):
			t.Fatalf("page %d fault was not served within the deadline (Serve wedged)", page)
		}
		return 0 // unreachable (t.Fatalf exits)
	}

	if got := readByte(frozenPage); got != forkTimeTag {
		t.Fatalf("frozen page served %q, want fork-time %q (leak tag is %q): the per-fault frozen composite did not isolate the child", string(got), string(forkTimeTag), string(leakTag))
	}
	if got := readByte(livePage); got != liveTag {
		t.Fatalf("live page served %q, want live source %q", string(got), string(liveTag))
	}
	if fc := h.FaultCount(); fc < 2 {
		t.Errorf("handler served %d faults, want at least 2 (frozen + live pages)", fc)
	}
}
