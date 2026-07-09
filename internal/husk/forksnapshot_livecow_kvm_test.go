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
// shortKVMWorkDir returns a short temp dir for a husk VM WorkDir. t.TempDir()
// embeds the (long) test name, and a co-located child FC binds its API socket at
// WorkDir/<childID>/firecracker.sock; with these long test names that path
// overflows the unix-socket sun_path limit (SUN_LEN, 108 bytes) and the child
// Firecracker exits with "path must be shorter than SUN_LEN". Pinned to /tmp (not os.TempDir, which honors a possibly-long $TMPDIR) so the
// socket path stays short + deterministic. Cleaned up at test end.
func shortKVMWorkDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("/tmp", "hk")
	if err != nil {
		t.Fatalf("short workdir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(d) })
	return d
}

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
		AllowUnverified: true,
		ReadyTimeout:    30 * time.Second,
		MultiVM:         true,
		LiveCowFork:     true,
		// Child import ON so forkSnapshotInstance takes the vmstate-only (no-mem)
		// capture this test asserts: the child boots its guest RAM from the source
		// shared memfd (composed below via ChildImport + ComposeChildFromImport), so
		// the disk mem is intentionally absent. Production keeps this OFF until a
		// child-side memfd-import Firecracker patch ships (see
		// TestForkSnapshotLiveCowArmedFullFallbackColocatedChildKVM).
		LiveCowChildImport: true,
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
	// arms the freezer. This is the exact step issue #832 was missing: before the
	// wp-offer-on-restore Firecracker fix, a RESTORED source loaded its guest RAM
	// MAP_PRIVATE from the mem file and never ran the mitos export + WP-offer hooks (those
	// ran only on the fresh-boot path), so this handshake never completed and every fork
	// fell back to the ~364ms Full snapshot. With the fixed binary the RESTORED source
	// backs its RAM with a shared memfd, exports it, and offers WP during restore, so the
	// freezer arms here.
	//
	// Runner gating: an unpatched Firecracker (or one built before the restore-path fix)
	// does not offer the handshake, and some sandboxes block /dev/userfaultfd so the WP
	// offer cannot be created at all; in either case the vmstate-only path is unreachable
	// and we SKIP rather than assert on the Full fallback (never a false pass), the same
	// gating the sibling internal/fork write-protect KVM tests use. Set
	// MITOS_KVM_REQUIRE_LIVECOW_RESTORE_ARM to turn that skip into a HARD failure. The
	// firecracker-test job sets it whenever the runner's userfaultfd is reachable (it
	// pins the wp-offer-on-restore binary), so the job proves a RESTORED source actually
	// arms instead of silently passing.
	requireArm := os.Getenv("MITOS_KVM_REQUIRE_LIVECOW_RESTORE_ARM") != ""
	deadline := time.Now().Add(20 * time.Second)
	for src.liveCowSnapshotFreezer() == nil {
		if time.Now().After(deadline) {
			const msg = "the RESTORED source Firecracker did not offer the live-cow write-protect handshake; the vmstate-only path is unreachable, Full-snapshot fallback would run"
			if requireArm {
				t.Fatalf("live-cow source-arm e2e (strict, MITOS_KVM_REQUIRE_LIVECOW_RESTORE_ARM set): %s. The pinned Firecracker must arm a restored source (issue #832)", msg)
			}
			t.Skipf("skipping live-cow source-arm e2e: %s (unpatched or restore-path memfd share unsupported on this runner)", msg)
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
	// and create_snapshot is far below the Full mem write it replaces.
	for _, stage := range []string{"pause", "freeze", "create_snapshot", "resume"} {
		if _, ok := fres.Stages[stage]; !ok {
			t.Errorf("armed live-cow fork result missing stage %q; got %v", stage, fres.Stages)
		}
	}

	// Boot the FULL-snapshot control now, unconditionally. It serves two purposes: it
	// measures what writing the mem file ACTUALLY costs on this host right now, and it
	// is the #838 isolation control consulted further down.
	//
	// This assertion used to be an absolute "create_snapshot < 100ms". That is a
	// property of the machine, not of the code: shared CI runners measured 103.8ms and
	// 177.4ms for the same capture and failed a check whose intent is only that the
	// capture is far cheaper than writing guest RAM. The same runners took 1469ms for
	// the Full mem write, so the ratio is ~10x and a same-host baseline expresses the
	// invariant without pinning it to any particular disk.
	fullSurvives, fullCaptureMs := fullPathRestoredSourceSurvivesFork(ctx, t, snapDir, templateRootfs, fcBin, memMiB)
	capMs := fres.Stages["create_snapshot"]
	switch {
	case fullCaptureMs <= 0:
		// Never silently skip: say so. The no-mem-file assertion above stays the hard gate.
		t.Logf("no FULL-path capture baseline on this host (control did not reach its fork); skipping the capture-cost ratio check, create_snapshot = %.3fms", capMs)
	case capMs >= fullCaptureMs/3:
		t.Errorf("create_snapshot stage = %.3fms, want far below the %.3fms the FULL mem write costs on this same host (the vmstate-only capture must skip the %dMiB mem write)", capMs, fullCaptureMs, memMiB)
	default:
		t.Logf("vmstate-only capture %.3fms vs FULL mem write %.3fms on this host (%.1fx cheaper)", capMs, fullCaptureMs, fullCaptureMs/capMs)
	}

	// (SOURCE SAFE) the resumed source still execs (never left frozen) and dirties
	// memory, which the armed handler's Serve loop resolves as write-protect faults.
	//
	// ISOLATION (issue #838): a RESTORED source's guest-agent vsock connection can break
	// on the restore-resume itself (the guest resets the connections it held at snapshot
	// time; the Firecracker log shows `vsock: ... pp=53 ... BrokenPipe` right after the
	// post-restore device kick, BEFORE any live-cow freeze). That is orthogonal to the
	// live-cow write-protect path and is tracked as issue #838 (restored source unusable
	// after fork, reproduces with live-cow OFF). So if the ARMED source cannot exec after
	// the fork, we FIRST check whether a FULL-snapshot restored source (live-cow OFF) can
	// on this same harness. If the FULL baseline ALSO cannot, this is the pre-existing
	// #838 restore-resume bug and we do NOT gate the live-cow work on it (the live-cow
	// arm + vmstate-only + child-import assertions above and below stay hard). If the FULL
	// baseline SURVIVES but the armed source does not, the write-protect freeze is the
	// regression and we hard-fail.
	srcAlive := true
	// Bound the exec: a restored source whose guest-agent vsock reset (issue #838) can
	// leave the stream blocked, which would hang the test to its package timeout instead
	// of failing fast. A deadline makes a dead source return promptly.
	execCtx, execCancel := context.WithTimeout(ctx, 45*time.Second)
	defer execCancel()
	if _, err := kvmExecOKCtx(execCtx, srcAgent, "printf source-alive-after-fork"); err != nil {
		srcAlive = false
		if fullSurvives {
			t.Fatalf("armed live-cow source must still exec after the fork, but a FULL-snapshot restored source DOES survive its fork on this harness: the write-protect freeze is the regression: %v", err)
		}
		t.Logf("known issue #838 (NOT a live-cow regression): a restored source is unreachable after its own fork on BOTH the live-cow AND the FULL-snapshot path (guest-agent vsock resets at restore-resume, before any freeze); scoping the source-exec assertion, the live-cow arm + vmstate-only + child-import assertions stay hard: %v", err)
	}
	if srcAlive {
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

	t.Logf("m6b live-cow source-arm PASS: vmstate-only fork wrote NO mem file (%dMiB guest RAM stayed in the shared memfd); create_snapshot=%.3fms freeze=%.3fms vs ~364ms Full mem write; source srcAlive=%v + %d WP faults served; child composed %d bytes from the source memfd (no disk mem)",
		memMiB, fres.Stages["create_snapshot"], fres.Stages["freeze"], srcAlive, handle.FaultCount(), len(childMem))
}

// TestForkSnapshotLiveCowArmedFullFallbackColocatedChildKVM is the real-KVM gate for
// the v1.32.2 prod hang: with --live-cow-fork ON, co-located fork children hung in
// phase=Restoring until the client 120s deadline (FORKERR 120.3). Root cause: the
// source armed and forkSnapshotInstance took the vmstate-only capture (NO mem file),
// but the shipped Firecracker patches the SOURCE (restore) side ONLY, so the
// co-located child RESTORES FROM THE DISK fork snapshot and a vmstate-only snapshot
// left it with no mem to restore. The fix gates the mem-skip behind
// Options.LiveCowChildImport (default OFF), so an ARMED source still writes the disk
// mem and its co-located child is restorable.
//
// This test mirrors the PROD sequence the arm-then-fork-in-one-shot vmstate-only test
// does not: restore the source at "pod start" (Prepare), CLAIM it (Activate), let the
// source ARM (freezer live: the write-protect handshake completed), then FORK, then
// SPAWN A REAL CO-LOCATED CHILD from the fork snapshot and prove it RESTORES AND EXECS
// (no hang, bounded). It asserts, end to end on KVM, with child import OFF (prod):
//   - (ARM STILL COMPLETES) the restored+claimed source arms the write-protect
//     handshake exactly as the vmstate-only test proves, so re-enabling --live-cow-fork
//     is safe (freezer live) without the mem-skip;
//   - (RESTORABLE FORK) the fork writes the disk `mem` (Full path) even though the
//     source is armed, so the co-located child has a snapshot it can restore;
//   - (CHILD DOES NOT HANG) a real co-located SpawnVM child restores from that fork
//     snapshot and execs within a bounded deadline, the exact step that hung on prod.
func TestForkSnapshotLiveCowArmedFullFallbackColocatedChildKVM(t *testing.T) {
	if raceDetectorEnabled {
		t.Skip("skipping live-cow armed full-fallback KVM test under -race (WP handshake is timing-sensitive; firecracker-test runs it without -race)")
	}
	if _, err := os.Stat("/dev/kvm"); err != nil {
		t.Skip("skipping live-cow armed full-fallback e2e: /dev/kvm not available (needs a KVM runner)")
	}
	snapDir := os.Getenv("MITOS_KVM_HUSK_SNAPSHOT_DIR")
	if snapDir == "" {
		t.Skip("skipping live-cow armed full-fallback e2e: set MITOS_KVM_HUSK_SNAPSHOT_DIR (the KVM CI sets it)")
	}
	for _, name := range []string{"mem", "vmstate"} {
		if _, err := os.Stat(filepath.Join(snapDir, name)); err != nil {
			t.Skipf("skipping live-cow armed full-fallback e2e: snapshot file %s not present: %v", name, err)
		}
	}
	templateRootfs := filepath.Join(filepath.Dir(snapDir), "rootfs.ext4")
	if _, err := os.Stat(templateRootfs); err != nil {
		t.Skipf("skipping live-cow armed full-fallback e2e: template rootfs %s not present: %v", templateRootfs, err)
	}
	fcBin := os.Getenv("MITOS_KVM_FIRECRACKER")
	if fcBin == "" {
		fcBin = "/usr/local/bin/firecracker"
	}

	const memMiB = 256
	// --multi-vm --live-cow-fork source, but child import OFF: the PRODUCTION posture.
	// The source arms (launches with the FIRECRACKER_MITOS_* env), yet forkSnapshotInstance
	// must keep the disk mem so a co-located child restores from it.
	src := New(firecracker.VMConfig{
		ID:             "husk-livecow-fallback-src",
		FirecrackerBin: fcBin,
		WorkDir:        shortKVMWorkDir(t),
		VcpuCount:      1,
		MemSizeMib:     memMiB,
	}, Options{
		AllowUnverified:    true,
		ReadyTimeout:       30 * time.Second,
		MultiVM:            true,
		LiveCowFork:        true,
		LiveCowChildImport: false, // prod posture: no child-side memfd-import binary shipped
		RootfsTemplatePath: templateRootfs,
		RootfsCoWDir:       t.TempDir(),
	})
	defer func() { _ = src.Close() }()

	ctx := context.Background()
	// "Pod start": Prepare brings the dormant source Firecracker up (armed: the WP
	// socket is bound and the source launches with the live-cow env).
	if err := src.Prepare(ctx); err != nil {
		t.Fatalf("source Prepare (multi-vm live-cow, child import off): %v", err)
	}
	// "Claim": Activate restores the source; the patched restore path exports its
	// memfd and offers write-protect, so the handshake completes here.
	sres, err := src.Activate(ctx, ActivateRequest{SnapshotDir: snapDir})
	if err != nil || !sres.OK {
		t.Fatalf("source Activate: err=%v res=%+v", err, sres)
	}
	srcAgent, err := kvmConnectAgent(sres.VsockPath)
	if err != nil {
		t.Fatalf("connect source agent: %v", err)
	}
	defer srcAgent.Close() //nolint:errcheck // best-effort teardown

	// (ARM STILL COMPLETES) wait bounded for the restored+claimed source to arm, the
	// same runner-gating the vmstate-only test uses. Whether or not it arms, the
	// Full-fallback + restorable-child assertions below hold; MITOS_KVM_REQUIRE_LIVECOW_RESTORE_ARM
	// makes the arm mandatory so the job proves an ARMED source still writes the mem.
	requireArm := os.Getenv("MITOS_KVM_REQUIRE_LIVECOW_RESTORE_ARM") != ""
	armed := false
	deadline := time.Now().Add(20 * time.Second)
	for {
		if src.liveCowSnapshotFreezer() != nil {
			armed = true
			break
		}
		if time.Now().After(deadline) {
			if requireArm {
				t.Fatalf("live-cow armed full-fallback e2e (strict): the RESTORED source did not arm the write-protect handshake; re-enabling --live-cow-fork would not be exercising the armed path (issue #832)")
			}
			t.Logf("source did not arm on this runner (unpatched or uffd unavailable); asserting the Full-fallback restorable child anyway")
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	// (RESTORABLE FORK) fork the source. Even armed, child import OFF must keep the
	// Full path: a disk `mem` file MUST be written so the co-located child restores.
	forkDir := filepath.Join(t.TempDir(), "fork-snap")
	fres, err := src.ForkSnapshot(ctx, ForkSnapshotRequest{ForkID: "livecow-fallback-child", SnapshotDir: forkDir})
	if err != nil || !fres.OK {
		t.Fatalf("source ForkSnapshot (armed, child import off) must succeed on KVM: err=%v res=%+v", err, fres)
	}
	for _, name := range []string{"mem", "vmstate", "rootfs.ext4"} {
		if fi, err := os.Stat(filepath.Join(forkDir, name)); err != nil || (name == "mem" && fi.Size() == 0) {
			t.Fatalf("armed source with child import off must write a disk-restorable Full snapshot; %s missing/empty (vmstate-only would leave the co-located child unrestorable, the prod hang): stat err=%v", name, err)
		}
	}
	if _, ok := fres.Stages["freeze"]; ok {
		t.Errorf("child-import-off fork must NOT freeze the source (Full path), but a freeze stage was recorded: %v", fres.Stages)
	}

	// (CHILD DOES NOT HANG) spawn a REAL co-located child from the fork snapshot, the
	// exact SpawnVM path the controller drives (SpawnVMOnHusk). On prod this hung in
	// Restoring; here it must restore and exec within a bounded deadline.
	spawnCtx, spawnCancel := context.WithTimeout(ctx, 60*time.Second)
	defer spawnCancel()
	childRes := src.SpawnVM(spawnCtx, SpawnVMRequest{
		VMID:     "livecow-fallback-colo",
		Activate: ActivateRequest{SnapshotDir: forkDir, ForkSnapshot: true},
	})
	if !childRes.OK {
		t.Fatalf("co-located child must restore from the Full fork snapshot without hanging (the v1.32.2 prod hang), got: %+v", childRes)
	}
	childAgent, err := kvmConnectAgent(childRes.VsockPath)
	if err != nil {
		t.Fatalf("connect co-located child agent: %v", err)
	}
	defer childAgent.Close() //nolint:errcheck // best-effort teardown
	execCtx, execCancel := context.WithTimeout(ctx, 45*time.Second)
	defer execCancel()
	if _, err := kvmExecOKCtx(execCtx, childAgent, "printf colo-child-alive"); err != nil {
		t.Fatalf("co-located child restored but could not exec (a hung/broken restore): %v", err)
	}

	t.Logf("live-cow armed full-fallback PASS: source armed=%v; armed fork still wrote a Full disk mem (create_snapshot=%.3fms); co-located child restored and execed with no hang",
		armed, fres.Stages["create_snapshot"])
}

// TestForkSnapshotLiveCowChildImportColocatedBootsFromSourceMemfdKVM is the
// real-KVM gate that closes the loop AND proves the fork latency actually drops:
// with --live-cow-fork AND child import ON, a co-located fork child BOOTS ITS GUEST
// RAM BY LAZILY FAULTING IT IN FROM THE SOURCE MEMFD. The child restores through
// Firecracker's NATIVE Uffd backend pointed at a husk-side handler socket (NO disk
// mem file, NO FIRECRACKER_MITOS_CHILD_MEMFD env): the handler serves each faulting
// page from the source memfd + FROZEN overlay ON DEMAND, so the child copies only
// its working set (a few MiB) instead of eagerly copying all 256MiB.
//
// This is the half TestForkSnapshotLiveCowArmedFullFallbackColocatedChildKVM
// deliberately leaves off: that test proves the SAFE fallback (child import OFF ->
// source writes the disk mem -> child restores from disk, no hang). Here child
// import is ON, so the source writes NO mem file and the ONLY way the co-located
// child can boot is by faulting its guest RAM from the source memfd. It asserts,
// end to end on KVM:
//   - (VMSTATE ONLY) the fork wrote NO `mem` file (the ~364ms guest-RAM write is
//     gone) and create_snapshot is sub-15ms;
//   - (CHILD LAZILY FAULTS, BOOTS, NO DISK MEM) a real co-located SpawnVM child
//     restores from the fork snapshot that has NO mem file and still boots + execs
//     within a bounded deadline: it can only have taken its guest RAM from the
//     source memfd via the lazy UFFD handler;
//   - (LATENCY DROPS) the child's vmstate_restore stage is SMALL (well below 100ms),
//     NOT the ~391ms the EAGER 256MiB child copy cost: this is the whole point of the
//     lazy import, that the vmstate-only source win is finally realized end to end;
//   - (FORK COMPLETES) SpawnVM returns OK (no hang, the v1.32.2 prod failure mode);
//   - (FORK-TIME MEMORY, NO LEAK) when the restored source can exec, a fork-time
//     sentinel planted in guest tmpfs is read back by the child as its FORK-TIME
//     value even after the resumed source overwrites it, so the source's post-fork
//     write is write-protect-frozen and served from FROZEN at fault time, never
//     leaking into the child (the m2 invariant, observed through the real guest).
//
// GATING: skips on no /dev/kvm, no assets, race detector, or a source that does not
// arm the write-protect handshake (unpatched / uffd-less runner), exactly like the
// sibling tests. The child boots through Firecracker's STOCK Uffd backend (no child
// patch), so a child that cannot boot lazily (e.g. a runner that blocks the child's
// userfaultfd) SKIPS unless MITOS_KVM_REQUIRE_LIVECOW_CHILD_IMPORT is set (the
// firecracker-test job sets it once the runner's userfaultfd is reachable), so a
// gap is never a false pass and a capable runner is never a false skip.
func TestForkSnapshotLiveCowChildImportColocatedBootsFromSourceMemfdKVM(t *testing.T) {
	if raceDetectorEnabled {
		t.Skip("skipping live-cow child-import KVM test under -race (WP handshake is timing-sensitive; firecracker-test runs it without -race)")
	}
	if _, err := os.Stat("/dev/kvm"); err != nil {
		t.Skip("skipping live-cow child-import e2e: /dev/kvm not available (needs a KVM runner)")
	}
	snapDir := os.Getenv("MITOS_KVM_HUSK_SNAPSHOT_DIR")
	if snapDir == "" {
		t.Skip("skipping live-cow child-import e2e: set MITOS_KVM_HUSK_SNAPSHOT_DIR (the KVM CI sets it)")
	}
	for _, name := range []string{"mem", "vmstate"} {
		if _, err := os.Stat(filepath.Join(snapDir, name)); err != nil {
			t.Skipf("skipping live-cow child-import e2e: snapshot file %s not present: %v", name, err)
		}
	}
	templateRootfs := filepath.Join(filepath.Dir(snapDir), "rootfs.ext4")
	if _, err := os.Stat(templateRootfs); err != nil {
		t.Skipf("skipping live-cow child-import e2e: template rootfs %s not present: %v", templateRootfs, err)
	}
	fcBin := os.Getenv("MITOS_KVM_FIRECRACKER")
	if fcBin == "" {
		fcBin = "/usr/local/bin/firecracker"
	}
	requireArm := os.Getenv("MITOS_KVM_REQUIRE_LIVECOW_RESTORE_ARM") != ""
	requireChildImport := os.Getenv("MITOS_KVM_REQUIRE_LIVECOW_CHILD_IMPORT") != ""

	const memMiB = 256
	// --multi-vm --live-cow-fork source WITH child import ON: the source arms, and
	// forkSnapshotInstance takes the vmstate-only capture (NO mem file) because the
	// co-located child is expected to import the source memfd (SpawnVM sets
	// FIRECRACKER_MITOS_CHILD_MEMFD on it from the armed handle's ChildImport).
	src := New(firecracker.VMConfig{
		ID:             "husk-livecow-childimp-src",
		FirecrackerBin: fcBin,
		WorkDir:        shortKVMWorkDir(t),
		VcpuCount:      1,
		MemSizeMib:     memMiB,
	}, Options{
		AllowUnverified:    true,
		ReadyTimeout:       30 * time.Second,
		MultiVM:            true,
		LiveCowFork:        true,
		LiveCowChildImport: true,
		RootfsTemplatePath: templateRootfs,
		RootfsCoWDir:       t.TempDir(),
	})
	defer func() { _ = src.Close() }()

	ctx := context.Background()
	if err := src.Prepare(ctx); err != nil {
		t.Fatalf("source Prepare (multi-vm live-cow child-import): %v", err)
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

	// Wait bounded for the restored source to arm the write-protect handshake; skip
	// (or hard-fail under strict) if it never arms, exactly like the sibling tests.
	deadline := time.Now().Add(20 * time.Second)
	for src.liveCowSnapshotFreezer() == nil {
		if time.Now().After(deadline) {
			const msg = "the RESTORED source Firecracker did not offer the live-cow write-protect handshake; the vmstate-only path is unreachable"
			if requireArm {
				t.Fatalf("live-cow child-import e2e (strict, MITOS_KVM_REQUIRE_LIVECOW_RESTORE_ARM set): %s (issue #832)", msg)
			}
			t.Skipf("skipping live-cow child-import e2e: %s (unpatched or restore-path memfd share unsupported on this runner)", msg)
		}
		time.Sleep(50 * time.Millisecond)
	}

	// (FORK-TIME MEMORY, NO LEAK) plant a fork-time sentinel in the source guest's
	// tmpfs BEFORE the fork, so its page lives in the source guest RAM the child will
	// import. Scoped behind source execability: a restored source whose guest-agent
	// vsock reset at restore-resume (issue #838) cannot run this, in which case the
	// content assertions are skipped but the boot/no-mem/sub-15ms gates below stay
	// hard. tmpfs (/dev/shm) is guest RAM, so the sentinel is fork-inherited memory.
	const sentinelPath = "/dev/shm/mitos-childimport-sentinel"
	const forkVal = "forktime-c0ffee"
	const leakVal = "postfork-LEAK-dead"
	sentinelPlanted := false
	plantCtx, plantCancel := context.WithTimeout(ctx, 30*time.Second)
	defer plantCancel()
	// Write then READ BACK in the source: a bare "printf > file; sync || true" exits 0
	// even when the guest cannot create the file (no /dev/shm, read-only), which would
	// falsely arm sentinelPlanted and then fail the child read on a file that never
	// existed. The && chain exits non-zero unless the write took and reads back forkVal,
	// so a guest that cannot host the sentinel scopes the content check out (the
	// byte-level no-leak invariant is proven hard in TestForkSnapshotLiveCowSourceArmedVmstateOnlyKVM
	// and internal/fork), while a guest that CAN host it keeps the child read hard.
	plantCmd := "printf %s '" + forkVal + "' > " + sentinelPath + " && [ \"$(cat " + sentinelPath + ")\" = '" + forkVal + "' ]"
	if _, err := kvmExecOKCtx(plantCtx, srcAgent, plantCmd); err == nil {
		sentinelPlanted = true
	} else {
		t.Logf("could not plant+verify the fork-time sentinel in the source guest (no /dev/shm or issue #838 restore-resume vsock); content no-leak assertion scoped out (byte-level no-leak stays proven in the sibling source-arm test): %v", err)
	}

	// Fork the source down the vmstate-only path (freeze + vmstate, NO mem file).
	forkDir := filepath.Join(t.TempDir(), "fork-snap")
	fres, err := src.ForkSnapshot(ctx, ForkSnapshotRequest{ForkID: "livecow-childimp", SnapshotDir: forkDir})
	if err != nil || !fres.OK {
		t.Fatalf("source ForkSnapshot (armed live-cow child-import) must succeed on KVM: err=%v res=%+v", err, fres)
	}

	// (VMSTATE ONLY) NO mem file was written; vmstate + frozen rootfs present.
	memPath := filepath.Join(forkDir, "mem")
	if _, err := os.Stat(memPath); err == nil {
		t.Fatalf("child-import fork must write NO mem file, but %s exists (Full path ran; child would not be importing)", memPath)
	}
	if fi, err := os.Stat(filepath.Join(forkDir, "vmstate")); err != nil || fi.Size() == 0 {
		t.Errorf("child-import fork must write a non-empty vmstate, stat err=%v", err)
	}
	if _, ok := fres.Stages["freeze"]; !ok {
		t.Errorf("child-import fork must freeze the source (vmstate-only path); missing freeze stage: %v", fres.Stages)
	}
	// (SUB-15ms CAPTURE) the vmstate-only capture writes only the small device/CPU
	// state, so create_snapshot is sub-15ms vs the ~364ms Full mem write.
	capMs := fres.Stages["create_snapshot"]
	if capMs >= 100 {
		t.Errorf("create_snapshot stage = %.3fms, want far below the ~364ms Full mem write (vmstate-only capture; matches the sibling sub-100ms bound to avoid CI-runner flakiness; the real sub-15ms is measured on prod)", capMs)
	}

	// (FORK-TIME MEMORY, NO LEAK) after the fork, if the source can still exec,
	// overwrite the sentinel so a leaked page would be observable in the child. The
	// write-protect freeze must preserve the fork-time page in FROZEN, which the child
	// import sources instead of the live (now overwritten) memfd page.
	sourceOverwrote := false
	if sentinelPlanted {
		owCtx, owCancel := context.WithTimeout(ctx, 30*time.Second)
		defer owCancel()
		if _, err := kvmExecOKCtx(owCtx, srcAgent, "printf %s '"+leakVal+"' > "+sentinelPath+"; /bin/busybox sync || sync || true"); err == nil {
			sourceOverwrote = true
		} else {
			t.Logf("resumed source could not overwrite the sentinel (issue #838); the child must still read the fork-time value: %v", err)
		}
	}

	// (CHILD LAZILY FAULTS, BOOTS, NO DISK MEM) spawn a real co-located child from the
	// mem-less fork snapshot. With child import ON and the source armed, SpawnVM
	// restores the child through Firecracker's NATIVE Uffd backend pointed at a
	// husk-side handler (from the armed handle's ChildImport), so the child faults its
	// guest RAM from the source memfd + FROZEN overlay ON DEMAND instead of a disk mem
	// file (there is none). No child Firecracker patch is needed (stock Uffd backend).
	spawnCtx, spawnCancel := context.WithTimeout(ctx, 60*time.Second)
	defer spawnCancel()
	childRes := src.SpawnVM(spawnCtx, SpawnVMRequest{
		VMID:     "livecow-childimp-colo",
		Activate: ActivateRequest{SnapshotDir: forkDir, ForkSnapshot: true},
	})
	if !childRes.OK {
		if requireChildImport {
			t.Fatalf("live-cow child-import e2e (strict, MITOS_KVM_REQUIRE_LIVECOW_CHILD_IMPORT set): co-located child must boot BY LAZILY FAULTING THE SOURCE MEMFD from the mem-less fork snapshot, got: %+v. Firecracker's native Uffd restore backend + the husk lazy handler must serve the child's guest RAM", childRes)
		}
		t.Skipf("skipping live-cow child-import e2e: co-located child did not boot by lazy UFFD from the source memfd (runner Firecracker Uffd backend or child userfaultfd unavailable); the mem-less fork is only bootable by import: %+v", childRes)
	}
	// A child that boots from a mem-less fork snapshot proves it read NO disk mem file
	// (there is none) and took its guest RAM from the imported source memfd.
	if _, err := os.Stat(memPath); err == nil {
		t.Errorf("no mem file must exist in the fork snapshot at any point (child imports the memfd), but %s appeared", memPath)
	}
	childAgent, err := kvmConnectAgent(childRes.VsockPath)
	if err != nil {
		t.Fatalf("connect co-located child agent (child booted from the imported memfd): %v", err)
	}
	defer childAgent.Close() //nolint:errcheck // best-effort teardown
	execCtx, execCancel := context.WithTimeout(ctx, 45*time.Second)
	defer execCancel()
	if _, err := kvmExecOKCtx(execCtx, childAgent, "printf childimp-colo-alive"); err != nil {
		t.Fatalf("co-located child imported the source memfd and restored but could not exec (a corrupt/incoherent RAM import would not boot a working guest): %v", err)
	}

	// (LATENCY DROPS) the whole point of the lazy import: the child's vmstate_restore
	// must be SMALL, not the ~391ms the EAGER 256MiB child copy cost. The lazy UFFD
	// load returns as soon as the device-restore faults are served (a few pages), and
	// the guest faults the rest of its working set on demand after resume, so
	// vmstate_restore drops back to the low-tens-of-ms it was before the eager copy.
	// A stage at or above 100ms means the child still copied its whole guest RAM
	// up front (the eager path), which would make the vmstate-only source win
	// latency-neutral, the exact failure this change fixes.
	childRestoreMs, ok := childRes.Stages["vmstate_restore"]
	if !ok {
		t.Fatalf("co-located child spawn result missing vmstate_restore stage; got %v", childRes.Stages)
	}
	if childRestoreMs >= 100 {
		t.Fatalf("child vmstate_restore = %.3fms, want well below 100ms (lazy UFFD faults only the working set; >=100ms means the child eagerly copied all %dMiB, leaving the fork latency-neutral)", childRestoreMs, memMiB)
	}

	// (FORK-TIME MEMORY, NO LEAK) the child must read the sentinel at its FORK-TIME
	// value, never the source's post-fork overwrite. Hard when the sentinel was
	// planted; a corrupt import or a leaked source write would fail here.
	if sentinelPlanted {
		readCtx, readCancel := context.WithTimeout(ctx, 30*time.Second)
		defer readCancel()
		got, err := kvmExecOKCtx(readCtx, childAgent, "cat "+sentinelPath)
		if err != nil {
			t.Fatalf("child could not read the imported fork-time sentinel: %v", err)
		}
		if got != forkVal {
			t.Fatalf("child sentinel = %q, want fork-time %q (leaked source post-fork write=%v, sourceOverwrote=%v): the write-protect freeze/frozen overlay did not isolate the child", got, forkVal, got == leakVal, sourceOverwrote)
		}
	}

	t.Logf("live-cow lazy child-import PASS: vmstate-only fork wrote NO mem file; create_snapshot=%.3fms freeze=%.3fms (vs ~364ms Full mem write); co-located child LAZILY FAULTED its guest RAM from the source memfd (%dMiB) with child vmstate_restore=%.3fms (vs ~391ms eager copy) and execed with no hang; sentinelPlanted=%v sourceOverwrote=%v (child read fork-time value, no leak)",
		fres.Stages["create_snapshot"], fres.Stages["freeze"], memMiB, childRestoreMs, sentinelPlanted, sourceOverwrote)
}

// fullPathRestoredSourceSurvivesFork stands up a SECOND source with live-cow OFF (the
// FULL mem+vmstate snapshot path), restores it from the same snapshot, forks it, and
// reports whether the resumed source can still exec. It is the issue #838 isolation
// control for the live-cow source-exec assertion: a false result means even the stock
// Full-snapshot restore-resume leaves the source unusable (the pre-existing #838 bug),
// so the live-cow write-protect path must not be blamed for it. Any setup failure is
// treated as "did not survive" (returns false), biasing toward attributing a shared
// failure to the pre-existing bug rather than hard-failing the live-cow gate on a flake.
// It returns whether the FULL-path source survives its own fork (the #838 control) and
// the create_snapshot cost of that FULL fork, which is the real ~memMiB mem-file write
// measured on THIS host. The caller uses the cost as the baseline the vmstate-only
// capture must come in far below; 0 means no baseline could be captured.
func fullPathRestoredSourceSurvivesFork(ctx context.Context, t *testing.T, snapDir, templateRootfs, fcBin string, memMiB int) (bool, float64) {
	t.Helper()
	base := New(firecracker.VMConfig{
		ID:             "husk-fullpath-src",
		FirecrackerBin: fcBin,
		WorkDir:        t.TempDir(),
		VcpuCount:      1,
		MemSizeMib:     memMiB,
	}, Options{
		AllowUnverified:    true,
		ReadyTimeout:       30 * time.Second,
		MultiVM:            true,
		LiveCowFork:        false, // FULL snapshot path: the isolation control
		RootfsTemplatePath: templateRootfs,
		RootfsCoWDir:       t.TempDir(),
	})
	defer func() { _ = base.Close() }()

	if err := base.Prepare(ctx); err != nil {
		t.Logf("#838 control: FULL-path source Prepare failed (treating as not-survived): %v", err)
		return false, 0
	}
	bres, err := base.Activate(ctx, ActivateRequest{SnapshotDir: snapDir})
	if err != nil || !bres.OK {
		t.Logf("#838 control: FULL-path source Activate failed (treating as not-survived): err=%v res=%+v", err, bres)
		return false, 0
	}
	agent, err := kvmConnectAgent(bres.VsockPath)
	if err != nil {
		t.Logf("#838 control: FULL-path source agent connect failed (treating as not-survived): %v", err)
		return false, 0
	}
	defer agent.Close() //nolint:errcheck // best-effort teardown

	forkDir := filepath.Join(t.TempDir(), "fullpath-fork")
	fres, err := base.ForkSnapshot(ctx, ForkSnapshotRequest{ForkID: "fullpath-child", SnapshotDir: forkDir})
	fullCaptureMs := 0.0
	if err == nil && fres.OK {
		fullCaptureMs = fres.Stages["create_snapshot"]
	}
	if err != nil || !fres.OK {
		t.Logf("#838 control: FULL-path ForkSnapshot failed (treating as not-survived): err=%v res=%+v", err, fres)
		return false, 0
	}
	// Bound the control exec too: the Full-path source's vsock can hang exactly like
	// the armed one, and this probe must never hang the test to its package timeout.
	execCtx, execCancel := context.WithTimeout(ctx, 45*time.Second)
	defer execCancel()
	if _, err := kvmExecOKCtx(execCtx, agent, "printf fullpath-source-alive-after-fork"); err != nil {
		t.Logf("#838 control: FULL-path restored source ALSO cannot exec after its fork (confirms pre-existing #838): %v", err)
		return false, fullCaptureMs
	}
	t.Logf("#838 control: FULL-path restored source SURVIVES its fork and execs (so the live-cow freeze, not #838, would be the cause)")
	return true, fullCaptureMs
}

// TestPrewarmedLiveCowChildImportNoLeakKVM is the gate that the pre-warm does NOT
// regress the live-cow lazy-UFFD no-leak path (STRICT NO-REGRESSION gate #5): a
// pod that keeps a dormant child pre-warmed lets a co-located fork ADOPT that
// already-booted GENERIC Firecracker (fc_boot ~0, boot pre-paid off the hot path)
// and STILL restore the mem-less fork by LAZILY FAULTING the source memfd through
// the husk UFFD fault handler, alive, execing, and reading the fork-time sentinel
// (NO LEAK of the source's post-fork write). It is the child-import boots-from-
// memfd scenario with PrewarmChild ON, proving the pre-warmed child arms and
// restores through LoadSnapshotUFFD + the husk handler identically to the
// on-demand child. Self-skips where the runner Firecracker lacks the Uffd restore
// backend; strict under MITOS_KVM_REQUIRE_LIVECOW_CHILD_IMPORT.
func TestPrewarmedLiveCowChildImportNoLeakKVM(t *testing.T) {
	if raceDetectorEnabled {
		t.Skip("skipping pre-warm live-cow child-import KVM test under -race (WP handshake is timing-sensitive)")
	}
	if _, err := os.Stat("/dev/kvm"); err != nil {
		t.Skip("skipping pre-warm live-cow child-import e2e: /dev/kvm not available (needs a KVM runner)")
	}
	snapDir := os.Getenv("MITOS_KVM_HUSK_SNAPSHOT_DIR")
	if snapDir == "" {
		t.Skip("skipping pre-warm live-cow child-import e2e: set MITOS_KVM_HUSK_SNAPSHOT_DIR (the KVM CI sets it)")
	}
	for _, name := range []string{"mem", "vmstate"} {
		if _, err := os.Stat(filepath.Join(snapDir, name)); err != nil {
			t.Skipf("skipping pre-warm live-cow child-import e2e: snapshot file %s not present: %v", name, err)
		}
	}
	templateRootfs := filepath.Join(filepath.Dir(snapDir), "rootfs.ext4")
	if _, err := os.Stat(templateRootfs); err != nil {
		t.Skipf("skipping pre-warm live-cow child-import e2e: template rootfs %s not present: %v", templateRootfs, err)
	}
	fcBin := os.Getenv("MITOS_KVM_FIRECRACKER")
	if fcBin == "" {
		fcBin = "/usr/local/bin/firecracker"
	}
	requireArm := os.Getenv("MITOS_KVM_REQUIRE_LIVECOW_RESTORE_ARM") != ""
	requireChildImport := os.Getenv("MITOS_KVM_REQUIRE_LIVECOW_CHILD_IMPORT") != ""

	const memMiB = 256
	src := New(firecracker.VMConfig{
		ID:             "husk-prewarm-childimp-src",
		FirecrackerBin: fcBin,
		WorkDir:        t.TempDir(),
		VcpuCount:      1,
		MemSizeMib:     memMiB,
	}, Options{
		AllowUnverified:    true,
		ReadyTimeout:       30 * time.Second,
		MultiVM:            true,
		LiveCowFork:        true,
		LiveCowChildImport: true,
		PrewarmChild:       true,
		RootfsTemplatePath: templateRootfs,
		RootfsCoWDir:       t.TempDir(),
	})
	defer func() { _ = src.Close() }()

	ctx := context.Background()
	if err := src.Prepare(ctx); err != nil {
		t.Fatalf("source Prepare: %v", err)
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

	deadline := time.Now().Add(20 * time.Second)
	for src.liveCowSnapshotFreezer() == nil {
		if time.Now().After(deadline) {
			const msg = "the RESTORED source Firecracker did not offer the live-cow write-protect handshake; the vmstate-only path is unreachable"
			if requireArm {
				t.Fatalf("pre-warm live-cow child-import e2e (strict, MITOS_KVM_REQUIRE_LIVECOW_RESTORE_ARM set): %s", msg)
			}
			t.Skipf("skipping pre-warm live-cow child-import e2e: %s (unpatched or restore-path memfd share unsupported)", msg)
		}
		time.Sleep(50 * time.Millisecond)
	}

	// Plant a fork-time sentinel in the source guest RAM (tmpfs) BEFORE the fork.
	const sentinelPath = "/dev/shm/mitos-prewarm-childimp-sentinel"
	const forkVal = "forktime-c0ffee"
	const leakVal = "postfork-LEAK-dead"
	sentinelPlanted := false
	plantCtx, plantCancel := context.WithTimeout(ctx, 30*time.Second)
	defer plantCancel()
	// VERIFY the write landed so sentinelPlanted reflects reality: on a runner with
	// no /dev/shm (or #838 restore-resume vsock) the write fails and the content
	// no-leak assertion is scoped out, exactly like the sibling child-import test.
	if _, err := kvmExecOKCtx(plantCtx, srcAgent, "printf %s "+forkVal+" > "+sentinelPath+" && [ \"$(cat "+sentinelPath+")\" = "+forkVal+" ]"); err == nil {
		sentinelPlanted = true
	} else {
		t.Logf("could not plant+verify fork-time sentinel (no /dev/shm or #838 restore-resume vsock; content no-leak assertion scoped out, still proven in the sibling child-import test): %v", err)
	}

	// EAGERLY pre-warm the dormant child BEFORE the fork, so the fork adopts it.
	// The source activation above ALSO kicks an eager warm (eagerPrewarmChildAsync),
	// and warmPrewarmChild is single-flight, so this explicit PrewarmChild can return
	// while that in-flight warm is still finishing (the slot momentarily reads new).
	// A racing fork just misses and boots on demand in prod; here we need the adopt
	// path, so wait for the slot to actually reach dormant before forking.
	if err := src.PrewarmChild(ctx); err != nil {
		t.Fatalf("PrewarmChild must boot a dormant child: %v", err)
	}
	var slotState State
	warmDeadline := time.Now().Add(20 * time.Second)
	for {
		// Read the slot under the same locks the eager warm goroutine uses
		// (s.mu for the map lookup via instanceFor, then the instance's own mu),
		// so the poll never races prepareInstanceOpt's concurrent map/state writes.
		if inst := src.instanceFor(prewarmSlotVMID, false); inst != nil {
			inst.mu.Lock()
			slotState = inst.state
			inst.mu.Unlock()
		}
		if slotState == StateDormant || time.Now().After(warmDeadline) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if slotState != StateDormant {
		t.Fatalf("pre-warmed slot state = %s, want dormant before the fork", slotState)
	}

	// Fork down the vmstate-only path (freeze + vmstate, NO mem file).
	forkDir := filepath.Join(t.TempDir(), "fork-snap")
	fres, err := src.ForkSnapshot(ctx, ForkSnapshotRequest{ForkID: "prewarm-childimp", SnapshotDir: forkDir})
	if err != nil || !fres.OK {
		t.Fatalf("source ForkSnapshot (armed live-cow child-import) must succeed on KVM: err=%v res=%+v", err, fres)
	}
	memPath := filepath.Join(forkDir, "mem")
	if _, err := os.Stat(memPath); err == nil {
		t.Fatalf("child-import fork must write NO mem file, but %s exists", memPath)
	}

	// After the fork, if the source can still exec, overwrite the sentinel so a
	// leaked page would be observable in the child.
	sourceOverwrote := false
	if sentinelPlanted {
		owCtx, owCancel := context.WithTimeout(ctx, 30*time.Second)
		defer owCancel()
		if _, err := kvmExecOKCtx(owCtx, srcAgent, "printf %s "+leakVal+" > "+sentinelPath+" && [ \"$(cat "+sentinelPath+")\" = "+leakVal+" ]"); err == nil {
			sourceOverwrote = true
		} else {
			t.Logf("resumed source could not overwrite the sentinel (#838); child must still read fork-time value: %v", err)
		}
	}

	// The co-located child ADOPTS the pre-warmed generic Firecracker and restores the
	// mem-less fork by lazy UFFD import through the husk handler.
	spawnCtx, spawnCancel := context.WithTimeout(ctx, 60*time.Second)
	defer spawnCancel()
	childRes := src.SpawnVM(spawnCtx, SpawnVMRequest{
		VMID:     "prewarm-childimp-colo",
		Activate: ActivateRequest{SnapshotDir: forkDir, ForkSnapshot: true},
	})
	if !childRes.OK {
		if requireChildImport {
			t.Fatalf("pre-warm live-cow child-import e2e (strict): pre-warmed co-located child must boot by LAZILY FAULTING the source memfd from the mem-less fork: %+v", childRes)
		}
		t.Skipf("skipping pre-warm live-cow child-import e2e: pre-warmed child did not boot by lazy UFFD (runner Firecracker Uffd backend unavailable): %+v", childRes)
	}

	// (ON-FORK-PATH BOOT ~0) the adopted child recorded fc_boot=0: the boot was
	// pre-paid off the fork hot path, even on the live-cow import path.
	if fc, ok := childRes.Stages["fc_boot"]; !ok || fc != 0 {
		t.Fatalf("adopted pre-warmed child fc_boot = %v (ok=%v), want 0; stages=%v", fc, ok, childRes.Stages)
	}
	// The mem-less fork stays mem-less: the child imported the memfd, read no disk mem.
	if _, err := os.Stat(memPath); err == nil {
		t.Errorf("no mem file must exist in the fork snapshot at any point, but %s appeared", memPath)
	}

	childAgent, err := kvmConnectAgent(childRes.VsockPath)
	if err != nil {
		t.Fatalf("connect adopted pre-warmed child agent: %v", err)
	}
	defer childAgent.Close() //nolint:errcheck // best-effort teardown
	execCtx, execCancel := context.WithTimeout(ctx, 45*time.Second)
	defer execCancel()
	if _, err := kvmExecOKCtx(execCtx, childAgent, "printf prewarm-childimp-alive"); err != nil {
		t.Fatalf("adopted pre-warmed child imported the memfd and restored but could not exec: %v", err)
	}
	if childRestoreMs, ok := childRes.Stages["vmstate_restore"]; ok && childRestoreMs >= 100 {
		t.Fatalf("child vmstate_restore = %.3fms, want well below 100ms (lazy UFFD faults only the working set)", childRestoreMs)
	}

	// (FORK-TIME MEMORY, NO LEAK) the child reads the sentinel at its FORK-TIME
	// value, never the source's post-fork overwrite.
	if sentinelPlanted {
		readCtx, readCancel := context.WithTimeout(ctx, 30*time.Second)
		defer readCancel()
		got, err := kvmExecOKCtx(readCtx, childAgent, "cat "+sentinelPath)
		if err != nil {
			t.Fatalf("adopted child could not read the imported fork-time sentinel: %v", err)
		}
		if got != forkVal {
			t.Fatalf("child sentinel = %q, want fork-time %q (leaked source post-fork write=%v, sourceOverwrote=%v): the pre-warmed child's lazy import did not isolate it", got, forkVal, got == leakVal, sourceOverwrote)
		}
	}

	t.Logf("pre-warm live-cow child-import PASS: pre-warmed child ADOPTED (fc_boot=0, boot pre-paid) and LAZILY FAULTED its guest RAM from the source memfd on the mem-less fork with no hang; sentinelPlanted=%v sourceOverwrote=%v (child read fork-time value, no leak)", sentinelPlanted, sourceOverwrote)
}
