package husk

import (
	"fmt"
	"os"
	"path/filepath"

	"mitos.run/mitos/internal/fork"
)

// Live copy-on-write (live-cow) fork wiring (milestone m4b). This is the husk
// side of making a CO-LOCATED fork child share the PARENT's resident guest memory
// through the patched Firecracker (MAP_SHARED memfd + userfaultfd write-protect,
// internal/fork/wpfork*.go) instead of restoring from the disk fork snapshot, so
// the hosted fork drops toward sub-100ms.
//
// What is wired in this increment (default OFF, canaried separately from
// --multi-vm):
//   - the parent-side write-protect fork engine (internal/fork.WPForkHandle): the
//     m2 correctness engine that freezes the parent at the fork point and serves
//     copy-before-unprotect so a RESUMED parent cannot leak a post-fork write into
//     a child. It is KVM-tested for the inheritance + no-leak invariant
//     (internal/fork/wpfork_kvm_test.go).
//   - the parent-launch primitive (fork.LiveCowParentEnv + firecracker VMConfig.Env):
//     the FIRECRACKER_MITOS_* env that switches the patched Firecracker onto the
//     memfd-share + write-protect offer. The patched binary is behavior-identical
//     to stock until these are set.
//   - this gate: the flag is stored and the co-located spawn path consults it.
//
// What lands NEXT (documented in docs/fork-correctness.md): the CHILD-side memfd
// import (booting the co-located child's guest RAM from the parent's live memory
// instead of the disk snapshot mem file) needs a matching Firecracker patch on
// the child restore side (the shipped fork patches the PARENT side) plus a KVM
// node to verify end-to-end. Until then a live-cow-enabled pod still restores the
// co-located child from the disk fork snapshot (fail-closed): turning the flag on
// never breaks a fork, it only opts into the new path where it is complete. Off is
// byte-for-byte the current disk co-location.

const (
	// liveCowWPSockName is the parent's write-protect handshake socket the WP
	// handler listens on and the patched Firecracker connects to
	// (FIRECRACKER_MITOS_WP_UDS), bound under the parent VM's workdir.
	liveCowWPSockName = "mitos-wp.sock"
	// liveCowMemExportName is the file the patched Firecracker writes its guest
	// memfd coordinates to (FIRECRACKER_MITOS_SHARED_MEM_EXPORT), under the parent
	// VM's workdir, which the WP handler reads to reach the parent's live memory.
	liveCowMemExportName = "mitos-memfd.export"
	// liveCowChildImportName is the file SpawnVM writes the child-import export
	// line to (fork.ChildMemfdImport.ExportLine) and names to the child Firecracker
	// via FIRECRACKER_MITOS_CHILD_MEMFD, so the child boots its guest RAM from the
	// parent's live shared memfd (m5). Written under the fork snapshot dir, the same
	// node-local trust boundary the fork child already restores its rootfs from.
	liveCowChildImportName = "mitos-child-memfd.export"
)

// SetLiveCowParent wires the armed parent-side live-cow WP handler for this pod's
// running source VM (milestone m5). When set AND the stub was started with
// --live-cow-fork, a co-located fork child spawn imports its guest RAM from the
// parent's live shared memfd instead of the disk snapshot mem file. Nil (the
// default) keeps every co-located child on the disk restore. The provider is the
// fork.WPForkHandle the parent-arm wiring creates; passing it here is the seam the
// parent-arm increment flips on.
func (s *Stub) SetLiveCowParent(p fork.ChildImportProvider) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.liveCowParent = p
}

// liveCowChildImportEnv assembles the FIRECRACKER_MITOS_CHILD_MEMFD environment a
// co-located fork child Firecracker must be launched with so its restore takes the
// guest-memory backing from the parent's live shared memfd (MAP_PRIVATE + FROZEN
// overlay) named in an export file, instead of the disk snapshot mem file. It
// returns:
//   - (env, nil) when an armed live-cow parent published a child import: the child
//     boots from the shared memfd (the disk mem path is still passed to the child
//     Firecracker, so an unpatched binary that ignores the env falls back to disk;
//     a patched child-restore binary prefers the memfd);
//   - (nil, nil) when no live-cow parent is armed: the child restores from disk;
//   - (nil, err) on a real failure assembling the import: SpawnVM logs and falls
//     back to the disk restore, so the flag never breaks a fork (fail-closed).
func (s *Stub) liveCowChildImportEnv(req ActivateRequest) ([]string, error) {
	// Read the armed parent under s.mu: SetLiveCowParent may arm it asynchronously
	// while a sibling VM's SpawnVM runs, and an interface value is two words, so an
	// unsynchronized read could tear. Take a stable local and release the lock
	// before the ChildImport I/O (which never re-takes s.mu).
	s.mu.Lock()
	parent := s.liveCowParent
	s.mu.Unlock()
	if parent == nil {
		return nil, nil
	}
	if req.SnapshotDir == "" {
		return nil, fmt.Errorf("live-cow child import: empty snapshot dir")
	}
	imp, err := parent.ChildImport(req.SnapshotDir)
	if err != nil {
		return nil, fmt.Errorf("live-cow child import: %w", err)
	}
	exportPath := filepath.Join(req.SnapshotDir, liveCowChildImportName)
	if err := os.WriteFile(exportPath, []byte(imp.ExportLine()+"\n"), 0o600); err != nil {
		return nil, fmt.Errorf("live-cow child import: write export %s: %w", exportPath, err)
	}
	return fork.ChildMemfdEnv(exportPath), nil
}

// LiveCowForkEnabled reports whether this pod was started with the live-cow fork
// path enabled (--live-cow-fork). Exported for the controller-driven status and
// for tests.
func (s *Stub) LiveCowForkEnabled() bool { return s.liveCowFork }

// liveCowForkApplies reports whether a spawn is a co-located fork child that the
// live-cow path would accelerate: the flag is on AND the activate is a fork
// snapshot (a child of a running source), not a fresh template activation. Pure,
// so the gate is unit tested without a VMM.
func (s *Stub) liveCowForkApplies(req ActivateRequest) bool {
	return s.liveCowFork && req.ForkSnapshot
}

// liveCowParentPaths returns the write-protect socket and memfd export paths for a
// parent VM launched under workDir. An empty workDir (the unit path) yields empty
// paths so no live-cow env is emitted.
func liveCowParentPaths(workDir string) (wpUDS, memExport string) {
	if workDir == "" {
		return "", ""
	}
	return filepath.Join(workDir, liveCowWPSockName), filepath.Join(workDir, liveCowMemExportName)
}

// liveCowParentEnv returns the FIRECRACKER_MITOS_* environment a live-cow PARENT
// Firecracker under workDir must be launched with (empty when the flag is off or
// the workdir is empty). It is only meaningful paired with an armed WP handler on
// the same socket; the launch wiring that pairs them lands with the child-import
// increment (see the file header).
func (s *Stub) liveCowParentEnv(workDir string) []string {
	if !s.liveCowFork {
		return nil
	}
	wpUDS, memExport := liveCowParentPaths(workDir)
	return fork.LiveCowParentEnv(wpUDS, memExport)
}
