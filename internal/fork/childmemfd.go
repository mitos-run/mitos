package fork

import (
	"fmt"
	"strconv"
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
// via /proc), where to reach the handler's LIVE frozen bitmap memfd
// (BitmapPID/BitmapFD via /proc; the child mmaps it MAP_SHARED so it reads the
// CURRENT per-page source selector at attach time, not a stale snapshot taken when
// the import was assembled), and the page size.
//
// Each memfd also carries its identity (st_ino + st_dev, captured by the parent
// which OWNS the real descriptor). The child re-fstats the descriptor it reopens
// and rejects any mismatch, so an exporter that exited and had its PID recycled to
// an unrelated process cannot trick the child into mapping a foreign descriptor
// (fail closed). Inode 0 is never a real memfd, so a zero identity is rejected.
//
// It is written to the EnvChildMemfd file by the parent's WP handler (ChildImport)
// and consumed by the child restore (ComposeChildFromImport on the Go side; the
// Firecracker child-restore patch reads the same line).
type ChildMemfdImport struct {
	ParentPID int
	ParentFD  int
	ParentIno uint64
	ParentDev uint64
	Bytes     uint64
	FrozenPID int
	FrozenFD  int
	FrozenIno uint64
	FrozenDev uint64
	BitmapPID int
	BitmapFD  int
	BitmapIno uint64
	BitmapDev uint64
	PageSize  uint64
}

// exportFields is the number of space-separated numeric fields ExportLine emits
// and ParseChildMemfdImport requires. All fields are non-negative integers, so the
// line has NO variable-length path component: there is nothing for a stray space to
// truncate, and a strict field count plus per-field integer parse rejects any
// malformed or trailing input (finding: fmt.Sscanf %s could silently truncate a
// path at the first space).
const exportFields = 14

// ExportLine renders the import as the single all-numeric line the EnvChildMemfd
// file carries. The order matches ParseChildMemfdImport and the Firecracker
// child-restore patch contract.
func (c ChildMemfdImport) ExportLine() string {
	return fmt.Sprintf("%d %d %d %d %d %d %d %d %d %d %d %d %d %d",
		c.ParentPID, c.ParentFD, c.ParentIno, c.ParentDev,
		c.Bytes,
		c.FrozenPID, c.FrozenFD, c.FrozenIno, c.FrozenDev,
		c.BitmapPID, c.BitmapFD, c.BitmapIno, c.BitmapDev,
		c.PageSize)
}

// ParseChildMemfdImport parses the ExportLine the parent's WP handler wrote to the
// EnvChildMemfd file. Pure, so both the Go child-restore wrapper and unit tests
// consume it without a live handler; the /proc mmap it feeds is Linux-only. It
// splits on whitespace and requires EXACTLY exportFields numeric tokens, so a line
// with a truncated, extra, or non-numeric field is rejected rather than silently
// accepted.
func ParseChildMemfdImport(s string) (ChildMemfdImport, error) {
	fields := strings.Fields(strings.TrimSpace(s))
	if len(fields) != exportFields {
		return ChildMemfdImport{}, fmt.Errorf("parse child memfd import %q: want %d space-separated numeric fields, got %d", s, exportFields, len(fields))
	}
	var v [exportFields]uint64
	for i, f := range fields {
		u, err := strconv.ParseUint(f, 10, 64)
		if err != nil {
			return ChildMemfdImport{}, fmt.Errorf("parse child memfd import %q: field %d (%q): %w", s, i, f, err)
		}
		v[i] = u
	}
	// The pid/fd fields land in int. Re-parse them from the original field string
	// with bitSize 31 so the result is bounded to [0, 2^31-1] AT PARSE TIME: that
	// fits int on every platform (int is at least 32 bits) and, unlike a bound
	// check on an already-widened uint64, lets the static integer-conversion
	// analyzer track the bound straight through to the int() conversion. A real pid
	// or fd is far below 2^31; a larger value is malformed input, rejected here.
	pidFD := func(idx int) (int, error) {
		u, err := strconv.ParseUint(fields[idx], 10, 31)
		if err != nil {
			return 0, fmt.Errorf("parse child memfd import %q: pid/fd field %d (%q): %w", s, idx, fields[idx], err)
		}
		return int(u), nil
	}
	parentPID, err := pidFD(0)
	if err != nil {
		return ChildMemfdImport{}, err
	}
	parentFD, err := pidFD(1)
	if err != nil {
		return ChildMemfdImport{}, err
	}
	frozenPID, err := pidFD(5)
	if err != nil {
		return ChildMemfdImport{}, err
	}
	frozenFD, err := pidFD(6)
	if err != nil {
		return ChildMemfdImport{}, err
	}
	bitmapPID, err := pidFD(9)
	if err != nil {
		return ChildMemfdImport{}, err
	}
	bitmapFD, err := pidFD(10)
	if err != nil {
		return ChildMemfdImport{}, err
	}
	c := ChildMemfdImport{
		ParentPID: parentPID, ParentFD: parentFD, ParentIno: v[2], ParentDev: v[3],
		Bytes:     v[4],
		FrozenPID: frozenPID, FrozenFD: frozenFD, FrozenIno: v[7], FrozenDev: v[8],
		BitmapPID: bitmapPID, BitmapFD: bitmapFD, BitmapIno: v[11], BitmapDev: v[12],
		PageSize:  v[13],
	}
	if err := c.validate(); err != nil {
		return ChildMemfdImport{}, fmt.Errorf("parse child memfd import %q: %w", s, err)
	}
	return c, nil
}

// validate rejects an import with a non-positive pid, negative fd, zero size, zero
// page size, or a zero memfd inode identity (no real memfd has inode 0, so a zero
// identity means the parent could not capture it and the child must fail closed
// rather than attach an unverifiable descriptor).
func (c ChildMemfdImport) validate() error {
	switch {
	case c.ParentPID <= 0 || c.FrozenPID <= 0 || c.BitmapPID <= 0:
		return fmt.Errorf("non-positive pid")
	case c.ParentFD < 0 || c.FrozenFD < 0 || c.BitmapFD < 0:
		return fmt.Errorf("negative fd")
	case c.Bytes == 0 || c.PageSize == 0:
		return fmt.Errorf("zero guest size or page size")
	case c.ParentIno == 0 || c.FrozenIno == 0 || c.BitmapIno == 0:
		return fmt.Errorf("zero memfd inode identity (fail closed)")
	}
	return nil
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
// (WPForkHandle). It yields the coordinates a co-located child needs to boot from
// the parent's live memory: the ChildMemfdImport pointing at the parent shared
// memfd, the FROZEN memfd, and the handler's LIVE frozen bitmap memfd (so the child
// reads the CURRENT per-page source selector at attach time, not a stale copy). The
// dir argument names the child's node-local snapshot dir (the trust boundary the
// child already restores from); the husk consults the provider on a co-located
// fork-child spawn; a nil provider (no armed parent) means the child falls back to
// the disk restore.
type ChildImportProvider interface {
	ChildImport(dir string) (ChildMemfdImport, error)
}
