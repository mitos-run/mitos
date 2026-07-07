package fork

import (
	"fmt"
	"strings"
)

// This file holds the platform-independent core of the Mitos live copy-on-write
// (live-cow) fork CHILD-side memfd import (milestone m5): the contract by which a
// co-located fork child boots its guest RAM from the PARENT's live resident
// memory (a MAP_PRIVATE of the parent's shared memfd) instead of restoring the
// memory image from the disk fork snapshot mem file. This is the counterpart of
// the parent-side write-protect engine (wpfork.go): the parent freezes at the
// fork point and preserves pre-write pages in a FROZEN memfd; the child selects,
// per page, between the live shared memfd (pages the parent never wrote, still at
// their fork-time value under copy-on-write) and the FROZEN memfd (pages the
// parent overwrote after the fork point, whose fork-time bytes the handler saved).
//
// Why a child-side contract at all. The shipped patched Firecracker
// (mitos-run/firecracker branch mitos/uffd-wp-v1.15.0) patches only the PARENT
// side: m1 backs the running guest RAM with a MAP_SHARED memfd and exports its
// coordinates; m2 offers the write-protect uffd to the handler. Its RESTORE path
// still maps the guest RAM MAP_PRIVATE from the on-disk snapshot mem file
// (src/vmm/src/vstate/memory.rs::snapshot_file). To boot a child from the shared
// memfd the child Firecracker must, at restore time, map the guest region
// MAP_PRIVATE from a PASSED memfd (the parent's) and overlay the frozen pages,
// while still restoring CPU + device vmstate from the snapshot vmstate file. That
// is the smallest missing Firecracker patch (FIRECRACKER_MITOS_CHILD_MEMFD); the
// memory-attach operation it performs is exactly composeChildGuestMemory
// (childmemfd_linux.go), which is KVM-tested here through the Go handler so the
// mechanism is proven independently of the Firecracker patch landing.
//
// SECURITY (internal/fork is a named-reviewer path). The child reads ONLY the
// parent's own exported guest memfd (read side of the m1 export, same trust
// boundary: the parent VM is in the SAME pod/node) and the handler's private
// FROZEN memfd; the MAP_PRIVATE means every child write is copy-on-write and
// invisible to the parent and to sibling children. It performs no host-path write
// and copies no secret out of the guest. The whole path is dark unless the
// LiveCowFork flag armed the parent (FIRECRACKER_MITOS_* env) and a co-located
// fork child spawn opted in; off, the child restores from the disk snapshot
// byte-for-byte. See docs/threat-model.md and docs/fork-correctness.md.

// EnvChildMemfd is the environment variable the CHILD Firecracker reads to boot
// its guest RAM from the parent's live shared memfd instead of the disk snapshot
// mem file. When set it names a file holding a ChildMemfdImport export line
// (ExportLine); absent, the child restores memory from disk exactly as stock. The
// name must match the Firecracker child-restore patch (the counterpart of
// EnvSharedMem / EnvWPUDS on the parent side).
const EnvChildMemfd = "FIRECRACKER_MITOS_CHILD_MEMFD"

// ChildMemfdImport is the full set of coordinates a co-located fork child needs to
// build its point-in-time guest memory from the parent's live memory: where to
// reach the parent's shared guest memfd (ParentPID/ParentFD via /proc), the total
// guest size, where to reach the handler's private FROZEN memfd (FrozenPID/FrozenFD
// via /proc), the path to the frozen bitmap (which pages to source from FROZEN),
// and the page size. It is written to the EnvChildMemfd file by the parent's WP
// handler (ChildImport) and consumed by the child restore (ComposeChildFromImport
// on the Go side; the Firecracker child-restore patch reads the same line).
type ChildMemfdImport struct {
	ParentPID  int
	ParentFD   int
	Bytes      uint64
	FrozenPID  int
	FrozenFD   int
	BitmapPath string
	PageSize   uint64
}

// ExportLine renders the import as the single line the EnvChildMemfd file carries:
// "<parentPID> <parentFD> <bytes> <frozenPID> <frozenFD> <pageSize> <bitmapPath>".
// The bitmap path is last so it may contain no spaces up to the final field; the
// husk derives it under the parent VM workdir, which is space-free.
func (c ChildMemfdImport) ExportLine() string {
	return fmt.Sprintf("%d %d %d %d %d %d %s",
		c.ParentPID, c.ParentFD, c.Bytes, c.FrozenPID, c.FrozenFD, c.PageSize, c.BitmapPath)
}

// ParseChildMemfdImport parses the ExportLine the parent's WP handler wrote to the
// EnvChildMemfd file. Pure, so both the Go child-restore wrapper and unit tests
// consume it without a live handler; the /proc mmap it feeds is Linux-only.
func ParseChildMemfdImport(s string) (ChildMemfdImport, error) {
	var c ChildMemfdImport
	n, err := fmt.Sscanf(strings.TrimSpace(s), "%d %d %d %d %d %d %s",
		&c.ParentPID, &c.ParentFD, &c.Bytes, &c.FrozenPID, &c.FrozenFD, &c.PageSize, &c.BitmapPath)
	if err != nil || n != 7 {
		return ChildMemfdImport{}, fmt.Errorf("parse child memfd import %q: want \"<parentPID> <parentFD> <bytes> <frozenPID> <frozenFD> <pageSize> <bitmapPath>\": %w", s, err)
	}
	if c.ParentPID <= 0 || c.ParentFD < 0 || c.Bytes == 0 || c.FrozenPID <= 0 || c.FrozenFD < 0 || c.PageSize == 0 || c.BitmapPath == "" {
		return ChildMemfdImport{}, fmt.Errorf("parse child memfd import %q: non-positive or empty field", s)
	}
	return c, nil
}

// ChildMemfdEnv returns the environment entries a co-located live-cow CHILD
// Firecracker must be launched with so its restore takes the guest-memory backing
// from the parent's shared memfd (MAP_PRIVATE) + the frozen overlay named by the
// exportPath file, instead of the disk snapshot mem file. An empty exportPath (the
// unavailable path) yields nil so the child falls back to the disk restore. Kept
// pure so the wiring is testable off Linux.
func ChildMemfdEnv(exportPath string) []string {
	if exportPath == "" {
		return nil
	}
	return []string{EnvChildMemfd + "=" + exportPath}
}

// ChildImportProvider is implemented by the parent's armed live-cow WP handler
// (WPForkHandle). It yields, into dir, the coordinates a co-located child needs to
// boot from the parent's live memory: it writes the current frozen bitmap into dir
// and returns the ChildMemfdImport pointing at the parent memfd, the FROZEN memfd,
// and that bitmap file. The husk consults it on a co-located fork-child spawn; a
// nil provider (no armed parent) means the child falls back to the disk restore.
type ChildImportProvider interface {
	ChildImport(dir string) (ChildMemfdImport, error)
}
