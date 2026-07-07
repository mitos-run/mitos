package husk

import (
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
)

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
