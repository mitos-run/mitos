//go:build linux

package fork

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// This is the Linux side of the live-cow CHILD-side memfd import (milestone m5):
// the memory-attach a co-located fork child performs to boot its guest RAM from
// the PARENT's live resident memory instead of the disk snapshot mem file. It is
// a faithful Go statement of what the Firecracker child-restore patch
// (FIRECRACKER_MITOS_CHILD_MEMFD) must do at restore time, so the mechanism is
// KVM-tested through this code path (wpfork_kvm_test.go) independently of the
// Firecracker patch landing.
//
// SECURITY (internal/fork is a named-reviewer path). See childmemfd.go: the child
// maps the parent's own exported guest memfd MAP_PRIVATE (every child write is
// copy-on-write, invisible to the parent and siblings) and the handler's private
// FROZEN memfd read-only; it writes nowhere on the host filesystem.

// composeChildGuestMemory builds a co-located fork child's point-in-time guest
// memory from the parent's live shared memfd (parentFd) and the handler's FROZEN
// memfd (frozenFd), selecting per page: a page the handler has marked frozen (the
// parent overwrote it after the fork point) is taken from FROZEN at its fork-time
// value; every other page is left as the MAP_PRIVATE view of the parent's shared
// memfd, which still holds the fork-time value (the page is either untouched or
// still write-protected in the parent). The MAP_PRIVATE mapping is O(1) and the
// frozen overlay copies only the handful of pages the parent actually clobbered,
// so the attach is orders of magnitude cheaper than reading a whole disk mem file
// (the sub-100ms win). The returned slice is the child's guest RAM; the caller
// owns it and must unix.Munmap it.
//
// This is the milestone's point-in-time correctness model, identical to the
// parent-side KVM test: the frozen bitmap must reflect the parent's writes at the
// instant of compose. Pinning the child image against parent writes that happen
// AFTER compose (over the full VM lifetime) is served by the running WP Serve loop
// plus the child re-consulting FROZEN, and is out of scope for the attach itself.
func composeChildGuestMemory(parentFd, frozenFd int, frozenBM []byte, bytes, pageSize uint64) ([]byte, error) {
	if pageSize == 0 {
		return nil, fmt.Errorf("childmemfd: zero page size")
	}
	if bytes == 0 {
		return nil, fmt.Errorf("childmemfd: zero guest size")
	}
	// MAP_PRIVATE the parent's live shared memfd: pages read at fork-time value,
	// every child write is copy-on-write and never touches the parent's memory.
	child, err := unix.Mmap(parentFd, 0, int(bytes), unix.PROT_READ|unix.PROT_WRITE, unix.MAP_PRIVATE)
	if err != nil {
		return nil, fmt.Errorf("childmemfd: MAP_PRIVATE parent memfd: %w", err)
	}
	// Read-only view of the FROZEN image to source clobbered pages from.
	frozen, err := unix.Mmap(frozenFd, 0, int(bytes), unix.PROT_READ, unix.MAP_SHARED)
	if err != nil {
		_ = unix.Munmap(child)
		return nil, fmt.Errorf("childmemfd: mmap frozen memfd: %w", err)
	}
	defer func() { _ = unix.Munmap(frozen) }()

	npages := (bytes + pageSize - 1) / pageSize
	for p := uint64(0); p < npages; p++ {
		if !testFrozenBit(frozenBM, p) {
			continue
		}
		off := p * pageSize
		end := off + pageSize
		if end > bytes {
			end = bytes
		}
		// Overlay the fork-time bytes the handler preserved. The write copies-on
		// -write into the child's private mapping only.
		copy(child[off:end], frozen[off:end])
	}
	return child, nil
}

// ComposeChildFromImport resolves a ChildMemfdImport (the coordinates the parent's
// WP handler wrote to the EnvChildMemfd file) into the child's guest memory: it
// reaches the parent's shared memfd and the handler's FROZEN memfd through
// /proc/<pid>/fd/<fd> (the same mechanism the parent-side handler uses to reach the
// parent's live memfd), reads the frozen bitmap file, and composes the point-in
// -time image. This is the operation the Firecracker child-restore patch performs;
// exposed in Go so the husk (and the KVM test) can drive it directly. The caller
// owns the returned mapping and must unix.Munmap it.
func ComposeChildFromImport(imp ChildMemfdImport) ([]byte, error) {
	parentFd, closeParent, err := openProcFd(imp.ParentPID, imp.ParentFD)
	if err != nil {
		return nil, fmt.Errorf("childmemfd: open parent memfd: %w", err)
	}
	defer closeParent()
	frozenFd, closeFrozen, err := openProcFd(imp.FrozenPID, imp.FrozenFD)
	if err != nil {
		return nil, fmt.Errorf("childmemfd: open frozen memfd: %w", err)
	}
	defer closeFrozen()
	bm, err := os.ReadFile(imp.BitmapPath)
	if err != nil {
		return nil, fmt.Errorf("childmemfd: read frozen bitmap %s: %w", imp.BitmapPath, err)
	}
	return composeChildGuestMemory(parentFd, frozenFd, bm, imp.Bytes, imp.PageSize)
}

// openProcFd re-opens another process's descriptor through /proc/<pid>/fd/<fd> and
// returns the local fd plus a close func. A memfd re-opened this way maps the same
// underlying pages, which is how a peer process reaches the parent Firecracker's
// guest memfd and the forkd handler's FROZEN memfd.
func openProcFd(pid, fd int) (int, func(), error) {
	path := fmt.Sprintf("/proc/%d/fd/%d", pid, fd)
	f, err := os.OpenFile(path, os.O_RDONLY, 0)
	if err != nil {
		return -1, nil, fmt.Errorf("open %s: %w", path, err)
	}
	// Dup so the returned fd outlives f.Close(); the caller closes via the func.
	dup, err := unix.Dup(int(f.Fd()))
	if err != nil {
		_ = f.Close()
		return -1, nil, fmt.Errorf("dup %s: %w", path, err)
	}
	_ = f.Close()
	return dup, func() { _ = unix.Close(dup) }, nil
}
