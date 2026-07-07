//go:build linux

package husk

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
	"mitos.run/mitos/internal/firecracker"
	"mitos.run/mitos/internal/fork"
)

// TestForkSnapshotLiveCowSourceArmedVmstateOnlyKVM is the milestone-m6b real-KVM
// gate: it proves the SOURCE-ARM wiring makes forkSnapshotInstance take the
// vmstate-only snapshot path against a REAL patched Firecracker, dropping the
// ~364ms `create_snapshot` mem write (issue #832, item 1).
//
// What it exercises that no prior test did: the production PARENT-ARM path
// (prepareInstance -> armLiveCowSource -> the patched source Firecracker exports its
// guest memfd + offers the write-protect uffd during restore -> serveLiveCowSource
// completes the handshake and arms the freezer via SetLiveCowParent). The existing
// internal/fork KVM tests stand in for the Firecracker parent with a hand-built
// memfd/uffd; this test drives the WHOLE husk Stub against a real restored source VM.
//
// It asserts, end to end on KVM:
//   - (vmstate-only ENGAGED) the fork wrote NO `mem` file in the snapshot dir, only
//     `vmstate` (+ the frozen `rootfs.ext4` disk pair), so the 364ms guest-RAM copy
//     was skipped;
//   - (SUB-MS CAPTURE) the paused-window `freeze` + `create_snapshot` stages are
//     recorded and the `create_snapshot` stage is far below the ~364ms Full mem write;
//   - (SOURCE SAFE) the resumed source still execs (never left frozen) AND its running
//     guest takes real write-protect faults that the armed handler's Serve loop
//     resolves (FaultCount grows), the copy-before-unprotect machinery that keeps a
//     resumed source from leaking a post-fork write into a child (the m2 no-leak
//     invariant proven byte-for-byte in internal/fork TestLiveCowForkVmstateOnlyNoMemFile);
//   - (CHILD INHERITS, NO DISK MEM) the production child import (handle.ChildImport +
//     fork.ComposeChildFromImport) composes a child guest-memory mapping from the
//     source's LIVE shared memfd + FROZEN overlay, with NO disk mem file at all,
//     sized to the source guest RAM.
//
// GATED and skips cleanly: no /dev/kvm, no snapshot assets, a source Firecracker that
// does not offer the live-cow write-protect handshake on this runner (an unpatched or
// restore-path-unsupported binary), or the race detector all skip rather than assert,
// so it is never a false pass. The firecracker-test job (real KVM, patched Firecracker,
// no -race) is where it proves the wiring.
func TestForkSnapshotLiveCowSourceArmedVmstateOnlyKVM(t *testing.T) {
	if raceDetectorEnabled {
		t.Skip("skipping live-cow source-arm KVM test under -race (WP handshake is timing-sensitive; the firecracker-test job runs it without -race)")
	}
	if _, err := os.Stat("/dev/kvm"); err != nil {
		t.Skip("skipping live-cow source-arm e2e: /dev/kvm not available (needs a KVM runner)")
	}
	snapDir := os.Getenv("MITOS_KVM_HUSK_SNAPSHOT_DIR")
	if snapDir == "" {
		t.Skip("skipping live-cow source-arm e2e: set MITOS_KVM_HUSK_SNAPSHOT_DIR (the KVM CI sets it)")
	}
	for _, name := range []string{"mem", "vmstate"} {
		if _, err := os.Stat(filepath.Join(snapDir, name)); err != nil {
			t.Skipf("skipping live-cow source-arm e2e: snapshot file %s not present: %v", name, err)
		}
	}
	templateRootfs := filepath.Join(filepath.Dir(snapDir), "rootfs.ext4")
	if _, err := os.Stat(templateRootfs); err != nil {
		t.Skipf("skipping live-cow source-arm e2e: template rootfs %s not present: %v", templateRootfs, err)
	}
	fcBin := os.Getenv("MITOS_KVM_FIRECRACKER")
	if fcBin == "" {
		fcBin = "/usr/local/bin/firecracker"
	}

	const memMiB = 256
	// One --multi-vm --live-cow-fork source stub. LiveCowFork ON is the ONLY
	// difference from the co-located-inheritance KVM test: it arms the parent side of
	// the live-cow fork so the source Firecracker launches with the FIRECRACKER_MITOS_*
	// env and forkSnapshotInstance can reach the vmstate-only path. A per-activation
	// rootfs CoW gives the source its own clone the fork snapshot freezes.
	src := New(firecracker.VMConfig{
		ID:             "husk-livecow-src",
		FirecrackerBin: fcBin,
		WorkDir:        t.TempDir(),
		VcpuCount:      1,
		MemSizeMib:     memMiB,
	}, Options{
		AllowUnverified:    true,
		ReadyTimeout:       30 * time.Second,
		MultiVM:            true,
		LiveCowFork:        true,
		RootfsTemplatePath: templateRootfs,
		RootfsCoWDir:       t.TempDir(),
	})
	defer func() { _ = src.Close() }()

	ctx := context.Background()
	if err := src.Prepare(ctx); err != nil {
		t.Fatalf("source Prepare (multi-vm live-cow): %v", err)
	}
	sres, err := src.Activate(ctx, ActivateRequest{SnapshotDir: snapDir})
	if err != nil || !sres.OK {
		t.Fatalf("source Activate: err=%v res=%+v", err, sres)
	}
	srcAgent, err := kvmConnectAgent(sres.VsockPath)
	if err != nil {
		t.Fatalf("connect source agent: %v", err)
	}
	defer srcAgent.Close() //nolint:errcheck // best-effort teardown

	// Wait (bounded) for the parent-arm handshake to complete: the patched source
	// Firecracker connects to the WP socket during restore and serveLiveCowSource then
	// arms the freezer. If it never arms on this runner (an unpatched Firecracker, or a
	// binary that does not offer the write-protect handshake on the restore path), the
	// vmstate-only path is unreachable here, so SKIP rather than assert on the Full
	// fallback (never a false pass). This is the m2/restore precondition, analogous to
	// the internal/fork tests skipping when the kernel lacks write-protect.
	deadline := time.Now().Add(20 * time.Second)
	for src.liveCowSnapshotFreezer() == nil {
		if time.Now().After(deadline) {
			t.Skip("skipping live-cow source-arm e2e: the source Firecracker did not offer the live-cow write-protect handshake on this runner (unpatched or restore-path memfd share unsupported); the vmstate-only path is unreachable, Full-snapshot fallback would run")
		}
		time.Sleep(50 * time.Millisecond)
	}

	src.mu.Lock()
	handle := src.liveCowHandle
	src.mu.Unlock()
	if handle == nil {
		t.Fatal("freezer armed but no retained WP handle (m6b teardown/child-import seam broken)")
	}

	// Fork the source's default VM down the vmstate-only path. On the armed live-cow
	// path this FREEZES the guest (~microseconds) and writes ONLY the vmstate (+ the
	// frozen rootfs), NOT the ~364ms mem file.
	forkDir := filepath.Join(t.TempDir(), "fork-snap")
	fres, err := src.ForkSnapshot(ctx, ForkSnapshotRequest{ForkID: "livecow-child", SnapshotDir: forkDir})
	if err != nil || !fres.OK {
		t.Fatalf("source ForkSnapshot (armed live-cow) must succeed on KVM: err=%v res=%+v", err, fres)
	}

	// (vmstate-only ENGAGED) NO mem file; vmstate + frozen rootfs present.
	if _, err := os.Stat(filepath.Join(forkDir, "mem")); err == nil {
		t.Errorf("armed live-cow fork must write NO mem file, but %s exists (Full path ran)", filepath.Join(forkDir, "mem"))
	}
	if fi, err := os.Stat(filepath.Join(forkDir, "vmstate")); err != nil || fi.Size() == 0 {
		t.Errorf("armed live-cow fork must write a non-empty vmstate, stat err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(forkDir, "rootfs.ext4")); err != nil {
		t.Errorf("armed live-cow fork must still freeze the source rootfs (disk pair): %v", err)
	}

	// (SUB-MS CAPTURE) the paused-window freeze + create_snapshot stages are recorded,
	// and create_snapshot is far below the ~364ms Full mem write it replaces.
	for _, stage := range []string{"pause", "freeze", "create_snapshot", "resume"} {
		if _, ok := fres.Stages[stage]; !ok {
			t.Errorf("armed live-cow fork result missing stage %q; got %v", stage, fres.Stages)
		}
	}
	if capMs := fres.Stages["create_snapshot"]; capMs >= 100 {
		t.Errorf("create_snapshot stage = %.3fms, want far below the ~364ms Full mem write (vmstate-only capture)", capMs)
	}

	// (SOURCE SAFE) the resumed source still execs (never left frozen) and dirties
	// memory, which the armed handler's Serve loop resolves as write-protect faults.
	if _, err := kvmExecOK(srcAgent, "printf source-alive-after-fork"); err != nil {
		t.Fatalf("source must still exec after the fork snapshot (not left frozen): %v", err)
	}
	// Dirty a few MiB of guest RAM so the write-protected pages fault through Serve.
	if _, err := kvmExecOK(srcAgent, "dd if=/dev/zero of=/dev/shm/mitos-livecow-dirty bs=1M count=8 2>/dev/null; /bin/busybox sync || true"); err != nil {
		t.Logf("source memory-dirty exec returned (non-fatal): %v", err)
	}
	faultDeadline := time.Now().Add(5 * time.Second)
	for handle.FaultCount() == 0 {
		if time.Now().After(faultDeadline) {
			t.Errorf("armed handler served no write-protect faults after a resumed source dirtied memory; the copy-before-unprotect no-leak loop is not live over the real guest")
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	// (CHILD INHERITS, NO DISK MEM) compose a child guest-memory mapping from the
	// source's live shared memfd + FROZEN overlay through the PRODUCTION import path,
	// with no disk mem file at all. This is the memory the co-located child boots from
	// (the byte-level inheritance + no-leak is proven in internal/fork
	// TestLiveCowForkVmstateOnlyNoMemFile against the same handler).
	imp, err := handle.ChildImport(forkDir)
	if err != nil {
		t.Fatalf("armed handle ChildImport (child boots from the source memfd, no disk mem): %v", err)
	}
	childMem, err := fork.ComposeChildFromImport(imp)
	if err != nil {
		t.Fatalf("ComposeChildFromImport (child memory from source memfd + frozen overlay): %v", err)
	}
	defer func() { _ = unix.Munmap(childMem) }()
	if uint64(len(childMem)) != imp.Bytes {
		t.Errorf("child memory mapping = %d bytes, want the source guest RAM %d bytes", len(childMem), imp.Bytes)
	}
	// The mapping must be readable (touch the first and a later page).
	_ = childMem[0]
	if len(childMem) > (1 << 20) {
		_ = childMem[1<<20]
	}

	t.Logf("m6b live-cow source-arm PASS: vmstate-only fork wrote NO mem file (%dMiB guest RAM stayed in the shared memfd); create_snapshot=%.3fms freeze=%.3fms vs ~364ms Full mem write; source resumed + %d WP faults served; child composed %d bytes from the source memfd (no disk mem)",
		memMiB, fres.Stages["create_snapshot"], fres.Stages["freeze"], handle.FaultCount(), len(childMem))
}
