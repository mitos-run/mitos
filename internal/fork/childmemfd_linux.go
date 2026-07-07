//go:build linux

package fork

import (
	"fmt"
	"os"
	"strings"

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

// frozenMemfdName and frozenBitmapName are the memfd_create names the parent WP
// handler gives its private FROZEN image and its live frozen bitmap. The child
// verifies the /proc/<pid>/fd/<fd> it reopens actually points at a memfd with the
// expected name (openProcFdVerified), so a reused PID handing back an unrelated
// descriptor fails closed instead of mapping the wrong memory (PID reuse on the
// child memory-attach path).
const (
	frozenMemfdName  = "mitos-frozen"
	frozenBitmapName = "mitos-frozen-bm"
)

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
// reaches the parent's shared memfd, the handler's FROZEN memfd, and the handler's
// LIVE frozen bitmap memfd through /proc/<pid>/fd/<fd> (the same mechanism the
// parent-side handler uses to reach the parent's live memfd), maps the bitmap
// MAP_SHARED so it reads the CURRENT per-page source selector, and composes the
// point-in-time image. This is the operation the Firecracker child-restore patch
// performs; exposed in Go so the husk (and the KVM test) can drive it directly. The
// caller owns the returned mapping and must unix.Munmap it.
//
// Each reopen is identity-verified against the (name, st_ino, st_dev) the parent
// captured for the descriptor it OWNS, so an exporter that exited with its PID
// recycled to an unrelated process cannot make the child attach a foreign fd
// (fail closed).
func ComposeChildFromImport(imp ChildMemfdImport) ([]byte, error) {
	// The parent guest memfd is created by Firecracker; its name is not fixed here,
	// so require only that the reopened target is a memfd (empty wantName) and that
	// its captured inode identity matches.
	parentFd, closeParent, err := openProcFdVerified(imp.ParentPID, imp.ParentFD, "", imp.ParentIno, imp.ParentDev)
	if err != nil {
		return nil, fmt.Errorf("childmemfd: open parent memfd: %w", err)
	}
	defer closeParent()
	frozenFd, closeFrozen, err := openProcFdVerified(imp.FrozenPID, imp.FrozenFD, frozenMemfdName, imp.FrozenIno, imp.FrozenDev)
	if err != nil {
		return nil, fmt.Errorf("childmemfd: open frozen memfd: %w", err)
	}
	defer closeFrozen()
	bmFd, closeBM, err := openProcFdVerified(imp.BitmapPID, imp.BitmapFD, frozenBitmapName, imp.BitmapIno, imp.BitmapDev)
	if err != nil {
		return nil, fmt.Errorf("childmemfd: open frozen bitmap memfd: %w", err)
	}
	defer closeBM()
	// MAP_SHARED the LIVE bitmap so the child reads the CURRENT frozen bit per page
	// at compose time. A page the parent's WP handler freezes between the import and
	// this attach is therefore sourced from FROZEN, never from the live memfd where
	// the parent's post-fork write has landed (the m2 no-leak invariant end to end).
	bmBytes := frozenBitmapBytes(imp.Bytes, imp.PageSize)
	if bmBytes == 0 {
		return nil, fmt.Errorf("childmemfd: zero frozen bitmap size")
	}
	bm, err := unix.Mmap(bmFd, 0, int(bmBytes), unix.PROT_READ, unix.MAP_SHARED)
	if err != nil {
		return nil, fmt.Errorf("childmemfd: mmap live frozen bitmap: %w", err)
	}
	defer func() { _ = unix.Munmap(bm) }()
	return composeChildGuestMemory(parentFd, frozenFd, bm, imp.Bytes, imp.PageSize)
}

// openProcFdVerified re-opens another process's descriptor through
// /proc/<pid>/fd/<fd> and returns the local fd plus a close func, AFTER verifying
// the reopened descriptor is the expected memfd. It guards the child memory-attach
// against a reused PID: if the exporter exited and its PID was recycled, the fd
// number may now point at an unrelated descriptor, so mapping it blindly could
// attach foreign memory. Two checks must both pass, else it fails closed:
//   - the /proc/<pid>/fd/<fd> symlink target is a memfd (and, when wantName is set,
//     that memfd's name matches);
//   - the OPENED descriptor's (st_ino, st_dev) match the identity the parent
//     captured for the descriptor it owns.
func openProcFdVerified(pid, fd int, wantName string, wantIno, wantDev uint64) (int, func(), error) {
	path := fmt.Sprintf("/proc/%d/fd/%d", pid, fd)
	link, err := os.Readlink(path)
	if err != nil {
		return -1, nil, fmt.Errorf("readlink %s: %w", path, err)
	}
	if err := verifyMemfdLink(link, wantName); err != nil {
		return -1, nil, fmt.Errorf("%s: %w", path, err)
	}
	f, err := os.OpenFile(path, os.O_RDONLY, 0)
	if err != nil {
		return -1, nil, fmt.Errorf("open %s: %w", path, err)
	}
	// Authoritative identity check on the actually-opened fd (fstat of the fd cannot
	// be raced the way the symlink can): the object open(2) returned must be the one
	// whose identity the parent captured.
	ino, dev, err := fdIdentity(int(f.Fd()))
	if err != nil {
		_ = f.Close()
		return -1, nil, fmt.Errorf("%s: %w", path, err)
	}
	if ino != wantIno || dev != wantDev {
		_ = f.Close()
		return -1, nil, fmt.Errorf("%s: memfd identity mismatch (got ino=%d dev=%d, want ino=%d dev=%d): exporter likely exited and the pid was recycled", path, ino, dev, wantIno, wantDev)
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

// verifyMemfdLink checks a /proc/<pid>/fd/<fd> readlink target names a memfd (the
// kernel formats it "/memfd:<name> (deleted)"), and when wantName is non-empty that
// the memfd name matches. It rejects any non-memfd target (a regular file, socket,
// or pipe a recycled PID might hold at that fd number).
func verifyMemfdLink(link, wantName string) error {
	const memfdPrefix = "/memfd:"
	if !strings.HasPrefix(link, memfdPrefix) {
		return fmt.Errorf("reopened fd is not a memfd (link %q)", link)
	}
	if wantName == "" {
		return nil
	}
	name := strings.TrimPrefix(link, memfdPrefix)
	if i := strings.Index(name, " ("); i >= 0 { // strip the trailing " (deleted)"
		name = name[:i]
	}
	if name != wantName {
		return fmt.Errorf("reopened memfd name %q != expected %q", name, wantName)
	}
	return nil
}

// fdIdentity returns the (st_ino, st_dev) of an open descriptor. It is the identity
// a peer captures for a memfd it owns and the child re-checks after reopen.
func fdIdentity(fd int) (ino, dev uint64, err error) {
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return 0, 0, fmt.Errorf("fstat fd %d: %w", fd, err)
	}
	return st.Ino, uint64(st.Dev), nil
}

// procFdIdentity opens /proc/<pid>/fd/<fd> just long enough to read its
// (st_ino, st_dev). The parent uses it to capture the identity of a peer's memfd
// (the guest memfd it does not own directly) at export time.
func procFdIdentity(pid, fd int) (ino, dev uint64, err error) {
	path := fmt.Sprintf("/proc/%d/fd/%d", pid, fd)
	f, err := os.OpenFile(path, os.O_RDONLY, 0)
	if err != nil {
		return 0, 0, fmt.Errorf("open %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	return fdIdentity(int(f.Fd()))
}
