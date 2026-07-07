package husk

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

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

// liveCowForkFreezer is the subset of the armed parent-side live-cow WP handle
// (fork.WPForkHandle) that forkSnapshotInstance needs at the fork point: FREEZE
// the source guest region (UFFD write-protect the whole live mapping, ~9us) so a
// RESUMED source cannot leak a post-fork write forward into a co-located child (the
// m2 no-leak invariant, docs/fork-correctness.md). The real armed handle satisfies
// BOTH this and fork.ChildImportProvider; a nil or non-freezing parent keeps the
// Full-snapshot (disk mem) fallback.
type liveCowForkFreezer interface {
	Freeze() (time.Duration, error)
}

// liveCowSnapshotFreezer returns the armed parent handle as a freezer when the
// live-cow fork path is on AND a parent that can freeze is armed; nil otherwise
// (the Full CreateSnapshot fallback). The freeze at the fork point is what lets
// forkSnapshotInstance capture ONLY the vmstate and SKIP the ~364ms mem-file copy
// (issue #832): the source RAM stays in the shared memfd the child imports (m5),
// and the freeze keeps a resumed source from mutating it out from under the child.
// Read under s.mu because SetLiveCowParent may arm the parent from a sibling VM's
// path concurrently.
func (s *Stub) liveCowSnapshotFreezer() liveCowForkFreezer {
	if !s.liveCowFork {
		return nil
	}
	s.mu.Lock()
	parent := s.liveCowParent
	s.mu.Unlock()
	fr, ok := parent.(liveCowForkFreezer)
	if !ok {
		return nil
	}
	return fr
}

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
// the same socket; armLiveCowSource pairs them.
func (s *Stub) liveCowParentEnv(workDir string) []string {
	if !s.liveCowFork {
		return nil
	}
	wpUDS, memExport := liveCowParentPaths(workDir)
	return fork.LiveCowParentEnv(wpUDS, memExport)
}

// armLiveCowSource arms the PARENT side of the live-cow fork for the SOURCE VM
// (milestone m6b), the final wiring step that makes forkSnapshotInstance reach the
// vmstate-only snapshot path (issue #832): it BINDS the write-protect handshake
// socket the patched source Firecracker connects to during guest-memory setup and
// returns the FIRECRACKER_MITOS_* env the source Firecracker must be LAUNCHED with
// so it exports its guest memfd (m1) and offers the write-protect uffd (m2). A
// background goroutine (serveLiveCowSource) completes the handshake once Firecracker
// connects, arms the freezer (SetLiveCowParent, so liveCowSnapshotFreezer stops
// returning nil), and runs the copy-before-unprotect fault loop for the life of the
// source, so a resumed source cannot leak a post-fork write into a co-located child.
//
// It returns the parent env to append to the source Firecracker launch, or nil.
// The handler is retained on the Stub (liveCowHandle) for teardown.
//
// FAIL-SAFE (a fork NEVER breaks): it arms ONLY when --live-cow-fork is on AND a
// real per-VM workdir exists (the production Firecracker launch). The unit/mock path
// (empty workdir), a bind failure, or a non-Linux host (StartWPForkHandler returns
// ErrLiveCowUnsupported) all return nil env and arm nothing, so the source launches
// stock and forkSnapshotInstance takes the Full CreateSnapshot(mem, vmstate) path.
// Turning the flag on can therefore never break a fork; it only opts a patched pod
// into the vmstate-only capture where the whole path is present.
func (s *Stub) armLiveCowSource(workDir string) []string {
	if !s.liveCowFork || workDir == "" {
		return nil
	}
	wpUDS, memExport := liveCowParentPaths(workDir)
	handle, err := fork.StartWPForkHandler(fork.WPForkConfig{UDSPath: wpUDS, MemExportPath: memExport})
	if err != nil {
		// No handler bound (off Linux, or a socket-bind failure): launch the source
		// stock so every fork takes the Full-snapshot fallback. Not fatal. The workdir
		// is a pod-local path, not a secret.
		slog.Warn("live-cow source arm skipped: WP handler did not bind; forks use the Full snapshot fallback",
			"workdir", workDir, "err", err)
		return nil
	}
	s.mu.Lock()
	s.liveCowHandle = handle
	s.mu.Unlock()
	go s.serveLiveCowSource(handle)
	return fork.LiveCowParentEnv(wpUDS, memExport)
}

// serveLiveCowSource completes the write-protect handshake with the patched source
// Firecracker and, on success, arms the freezer and runs the fault loop for the life
// of the source VM. It runs in its own goroutine (armLiveCowSource starts it) so the
// blocking Receive/Serve never sit on a lifecycle lock.
//
// FAIL-SAFE: if the source Firecracker never offers the write-protect handshake (an
// unpatched binary, or it fell back to the paused-parent contract), Receive errors
// and the freezer is never armed, so liveCowSnapshotFreezer stays nil and every fork
// takes the Full-snapshot path. Serve is only started AFTER a successful handshake;
// it blocks harmlessly until the first Freeze write-protects the region, then serves
// copy-before-unprotect faults, and returns when the handler is Closed at teardown.
func (s *Stub) serveLiveCowSource(handle fork.WPForkHandle) {
	if err := handle.Receive(); err != nil {
		slog.Warn("live-cow source arm incomplete: write-protect handshake not received; forks use the Full snapshot fallback",
			"err", err)
		return
	}
	// Arm the freezer BEFORE Serve: forkSnapshotInstance can now take the vmstate-only
	// path (Freeze + CreateSnapshotVMStateOnly) instead of the 364ms Full mem write.
	s.SetLiveCowParent(handle)
	if err := handle.Serve(); err != nil {
		slog.Warn("live-cow source fault loop ended with error", "err", err)
	}
}

// closeLiveCowSource tears down the armed source-side WP handler at teardown,
// unblocking a stuck Receive (AcceptUnix returns) and stopping the Serve fault loop.
// A no-op when the source was never armed. Safe to call once, under no lock.
func (s *Stub) closeLiveCowSource() {
	s.mu.Lock()
	handle := s.liveCowHandle
	s.liveCowHandle = nil
	s.mu.Unlock()
	if handle != nil {
		_ = handle.Close()
	}
}
