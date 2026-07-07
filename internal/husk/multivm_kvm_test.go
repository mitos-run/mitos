package husk

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"mitos.run/mitos/internal/firecracker"
	"mitos.run/mitos/internal/guestgrpc"
	sandboxv1 "mitos.run/mitos/proto/sandbox/v1"
)

// TestMultiVMTwoRealFirecrackersKVM is the KVM acceptance bar for L1.4: with the
// multi-VM mode ON, ONE husk Stub brings up TWO distinct vmIDs, each spawning its
// OWN real Firecracker process (its own workdir, API socket, and vsock UDS via
// deriveVMConfig), both reaching StateActive from the same template snapshot, and
// each exec-ing INDEPENDENTLY over its OWN vsock. This is the only proof that a
// real second Firecracker coexists with the first in one pod; the mock cannot
// spawn a real VMM. The per-VM tap/IP DERIVATION that keeps their egress from
// crossing is proven deterministically by the mock-level tests
// (TestDeriveVMNetworkDistinctPerVMID, TestMultiVMActivateProgramsDistinctTapPerVMID);
// this test does not program the in-pod nftables datapath (no netRunner), so a
// full real two-tap-in-one-netns egress-isolation integration is L1.4b.
//
// It is GATED and skips cleanly unless /dev/kvm exists AND the asset env vars are
// set, mirroring internal/fork's KVM tests: on a developer darwin box or any
// non-KVM runner it never asserts, so it is never a fake pass. The KVM CI job
// (kvm-test.yaml) provides /dev/kvm, a real Firecracker, and a booted template
// snapshot, and sets:
//
//	MITOS_KVM_HUSK_SNAPSHOT_DIR  directory holding the template snapshot mem+vmstate
//	MITOS_KVM_FIRECRACKER        path to the firecracker binary (default /usr/local/bin/firecracker)
func TestMultiVMTwoRealFirecrackersKVM(t *testing.T) {
	if _, err := os.Stat("/dev/kvm"); err != nil {
		t.Skip("skipping multi-VM two-Firecracker proof: /dev/kvm not available (needs a KVM runner)")
	}
	snapDir := os.Getenv("MITOS_KVM_HUSK_SNAPSHOT_DIR")
	if snapDir == "" {
		t.Skip("skipping multi-VM two-Firecracker proof: set MITOS_KVM_HUSK_SNAPSHOT_DIR (the KVM CI sets it)")
	}
	for _, name := range []string{"mem", "vmstate"} {
		if _, err := os.Stat(filepath.Join(snapDir, name)); err != nil {
			t.Skipf("skipping multi-VM two-Firecracker proof: snapshot file %s not present: %v", name, err)
		}
	}
	fcBin := os.Getenv("MITOS_KVM_FIRECRACKER")
	if fcBin == "" {
		fcBin = "/usr/local/bin/firecracker"
	}

	// One MultiVM stub, its base workdir under the test temp dir. Each non-default
	// vmID nests its own workdir + sockets under it (deriveVMConfig). AllowUnverified
	// mirrors the husk-stub KVM activation phase: the CI snapshot carries no signed
	// manifest, so verification is disabled for the test, not the production gate.
	s := New(firecracker.VMConfig{
		ID:             "husk-kvm",
		FirecrackerBin: fcBin,
		WorkDir:        t.TempDir(),
		VcpuCount:      1,
		MemSizeMib:     256,
	}, Options{
		AllowUnverified: true,
		MultiVM:         true,
	})
	defer func() { _ = s.Close() }()

	ids := []vmID{defaultVMID, "vm-2"}
	vsockByID := map[vmID]string{}
	for _, id := range ids {
		if err := s.prepareInstance(context.Background(), id, "", nil); err != nil {
			t.Fatalf("prepareInstance(%s): %v", id, err)
		}
		res, err := s.activateInstance(context.Background(), id, ActivateRequest{SnapshotDir: snapDir})
		if err != nil || !res.OK {
			t.Fatalf("activateInstance(%s): err=%v ok=%v error=%s", id, err, res.OK, res.Error)
		}
		if res.VsockPath == "" {
			t.Fatalf("activateInstance(%s): no vsock path", id)
		}
		vsockByID[id] = res.VsockPath
	}

	// Distinct real Firecracker processes: their vsock UDS paths differ (each VM
	// bound its own socket under its own workdir), which is the host-side proof the
	// two VMs are separate processes, not one shared handle.
	if vsockByID[ids[0]] == vsockByID[ids[1]] {
		t.Fatalf("the two VMs must own distinct vsock paths, both = %q", vsockByID[ids[0]])
	}

	// Each VM execs INDEPENDENTLY over its OWN vsock. Write a distinct marker to
	// the SAME path in EVERY VM FIRST, then read them all back, so a regression
	// where two VMs shared a rootfs backing (one clobbering the other's file) is
	// caught: each VM must still read its OWN marker. Reading back in the same VM
	// immediately after writing would not detect crossing, since the last write
	// would win the shared file; writing all then reading all does.
	clients := map[vmID]*guestgrpc.Client{}
	for _, id := range ids {
		client, err := kvmConnectAgent(vsockByID[id])
		if err != nil {
			t.Fatalf("connect agent for %s: %v", id, err)
		}
		defer client.Close() //nolint:errcheck // best-effort teardown
		clients[id] = client

		marker := fmt.Sprintf("marker-%s", id)
		if _, err := kvmExecOK(client, fmt.Sprintf("printf %s > /vm-marker.txt", marker)); err != nil {
			t.Fatalf("write marker in %s: %v", id, err)
		}
	}
	for _, id := range ids {
		marker := fmt.Sprintf("marker-%s", id)
		got, err := kvmExecOK(clients[id], "cat /vm-marker.txt")
		if err != nil {
			t.Fatalf("read marker in %s: %v", id, err)
		}
		if strings.TrimSpace(got) != marker {
			t.Fatalf("%s read back %q, want %q (VMs shared a backing or exec crossed?)", id, strings.TrimSpace(got), marker)
		}
	}
}

// TestMultiVMForkSnapshotDefaultVMKVM is the KVM acceptance bar for the L1.8 prod
// canary: it FORKS a --multi-vm husk stub's DEFAULT VM end to end on a REAL
// Firecracker (prepare -> activate -> fork-snapshot), then proves the produced
// child snapshot is a real, restorable checkpoint by activating a SECOND --multi-vm
// stub from it and exec-ing in the restored guest. TestMultiVMTwoRealFirecrackersKVM
// above only proves two VMs ACTIVATE; it never forks a multi-vm stub, which is
// exactly the coverage gap that let the bug ship.
//
// The bug: under --multi-vm the CLAIM path (Activate) advances the DEFAULT
// INSTANCE's state, not the single-VM s.state, which stays StateNew. On origin/main
// ForkSnapshot gated on s.state and so refused EVERY fork of a multi-vm source with
// "fork-snapshot in state new: must be active", timing out the hosted fork loop.
// After the fix ForkSnapshot routes through the default instance and succeeds here.
//
// It is GATED and skips cleanly unless /dev/kvm exists AND the SAME asset env vars
// TestMultiVMTwoRealFirecrackersKVM uses are set (MITOS_KVM_HUSK_SNAPSHOT_DIR, plus
// MITOS_KVM_FIRECRACKER), so on a darwin dev box or any non-KVM runner it never
// asserts and is never a fake pass.
func TestMultiVMForkSnapshotDefaultVMKVM(t *testing.T) {
	if _, err := os.Stat("/dev/kvm"); err != nil {
		t.Skip("skipping multi-vm fork-snapshot e2e: /dev/kvm not available (needs a KVM runner)")
	}
	snapDir := os.Getenv("MITOS_KVM_HUSK_SNAPSHOT_DIR")
	if snapDir == "" {
		t.Skip("skipping multi-vm fork-snapshot e2e: set MITOS_KVM_HUSK_SNAPSHOT_DIR (the KVM CI sets it)")
	}
	for _, name := range []string{"mem", "vmstate"} {
		if _, err := os.Stat(filepath.Join(snapDir, name)); err != nil {
			t.Skipf("skipping multi-vm fork-snapshot e2e: snapshot file %s not present: %v", name, err)
		}
	}
	fcBin := os.Getenv("MITOS_KVM_FIRECRACKER")
	if fcBin == "" {
		fcBin = "/usr/local/bin/firecracker"
	}

	// newStub builds a real --multi-vm husk stub, each with its OWN base workdir so
	// the source and the child never collide on a Firecracker API socket or vsock
	// UDS. Same machine sizing and AllowUnverified as TestMultiVMTwoRealFirecrackersKVM.
	newStub := func(id string) *Stub {
		workdir := filepath.Join(t.TempDir(), id)
		if err := os.MkdirAll(workdir, 0o755); err != nil {
			t.Fatalf("mkdir workdir for %s: %v", id, err)
		}
		return New(firecracker.VMConfig{
			ID:             id,
			FirecrackerBin: fcBin,
			WorkDir:        workdir,
			VcpuCount:      1,
			MemSizeMib:     256,
		}, Options{
			AllowUnverified: true,
			ReadyTimeout:    30 * time.Second,
			MultiVM:         true,
		})
	}

	ctx := context.Background()

	// Source: a --multi-vm stub whose DEFAULT VM is activated from the template
	// snapshot. Under --multi-vm the single-VM s.state stays StateNew (activateInstance
	// advanced the DEFAULT INSTANCE), which is the state ForkSnapshot must NOT read.
	src := newStub("husk-fork-src")
	defer func() { _ = src.Close() }()
	if err := src.Prepare(ctx); err != nil {
		t.Fatalf("source Prepare (multi-vm default): %v", err)
	}
	if res, err := src.Activate(ctx, ActivateRequest{SnapshotDir: snapDir}); err != nil || !res.OK {
		t.Fatalf("source Activate (multi-vm default): err=%v res=%+v", err, res)
	}
	if src.state != StateNew {
		t.Fatalf("precondition: single-VM s.state must stay StateNew under multi-vm, got %s", src.state)
	}

	// Fork the source's default VM. On origin/main this returned OK=false with
	// "fork-snapshot in state new: must be active"; after the fix it routes through
	// the default instance and writes a restorable child checkpoint.
	childDir := filepath.Join(t.TempDir(), "child-snapshot")
	fres, err := src.ForkSnapshot(ctx, ForkSnapshotRequest{ForkID: "child-1", SnapshotDir: childDir})
	if err != nil || !fres.OK {
		t.Fatalf("ForkSnapshot of a multi-vm stub's default VM must succeed on KVM: err=%v res=%+v", err, fres)
	}
	for _, name := range []string{"mem", "vmstate"} {
		if _, err := os.Stat(filepath.Join(childDir, name)); err != nil {
			t.Fatalf("fork snapshot did not write %s: %v", name, err)
		}
	}

	// Child: a fresh --multi-vm stub activates its DEFAULT VM from the fork snapshot
	// and its guest answers, proving the fork of a multi-vm stub actually restores.
	child := newStub("husk-fork-child")
	defer func() { _ = child.Close() }()
	if err := child.Prepare(ctx); err != nil {
		t.Fatalf("child Prepare (from fork snapshot): %v", err)
	}
	cres, err := child.Activate(ctx, ActivateRequest{SnapshotDir: childDir})
	if err != nil || !cres.OK {
		t.Fatalf("child Activate from the fork snapshot must succeed: err=%v res=%+v", err, cres)
	}
	client, err := kvmConnectAgent(cres.VsockPath)
	if err != nil {
		t.Fatalf("connect agent in the forked child: %v", err)
	}
	defer client.Close() //nolint:errcheck // best-effort teardown
	got, err := kvmExecOK(client, "printf forked-child-alive")
	if err != nil {
		t.Fatalf("exec in the forked child guest: %v", err)
	}
	if strings.TrimSpace(got) != "forked-child-alive" {
		t.Fatalf("forked child guest exec = %q, want %q (the restored fork must run)", strings.TrimSpace(got), "forked-child-alive")
	}
}

// TestColocatedForkInheritsSourceStateKVM is the KVM acceptance bar for the
// prod-canary co-located-fork correctness bug: with --multi-vm ON, a hosted fork
// that CO-LOCATES the child as an ADDITIONAL VM INSIDE the source pod (the spawn-vm
// path the controller drives via SpawnVM) MUST restore the PARENT'S memory AND disk,
// not boot a fresh template VM.
//
// The bug: the co-located child was spawned from the pool TEMPLATE snapshot (and its
// rootfs cloned from the template rootfs), so a file written to the parent AND the
// parent's live in-memory state were BOTH ABSENT in the child. The co-location was
// fast precisely because it SKIPPED the restore a fork requires. The fix routes the
// spawn-vm ActivateRequest at the FORK snapshot (mem + vmstate + the FROZEN source
// rootfs at SnapshotDir/rootfs.ext4) with ForkSnapshot=true, so the child inherits
// the parent.
//
// This test proves BOTH the bug and the fix ON THE SAME KVM RUNNER by spawning two
// co-located children in one source stub:
//
//   - a BUG-REPRO child from the TEMPLATE snapshot (ForkSnapshot=false), the exact
//     request the old wiring sent: it inherits NEITHER the marker file NOR the
//     advanced uptime, and
//   - a FIXED child from the FORK snapshot (ForkSnapshot=true), the request the fix
//     sends: it reads back the marker file (DISK inheritance from the frozen source
//     rootfs) AND a MUCH larger /proc/uptime (MEMORY inheritance from the fork memory
//     checkpoint; uptime lives only in guest RAM and is never on disk).
//
// The mock/envtest fakes never restore real state, so this KVM proof is the only
// gate that catches the bug; the controller-level envtest asserts the WIRING (the
// spawn carries the parent fork snapshot dir + ForkSnapshot, not the template).
//
// GATED and skips cleanly unless /dev/kvm exists AND MITOS_KVM_HUSK_SNAPSHOT_DIR is
// set AND the template rootfs is present next to the snapshot (the bench template
// lays it out at <snapshot dir>/../rootfs.ext4), so a darwin dev box or any non-KVM
// runner never asserts and is never a fake pass.
func TestColocatedForkInheritsSourceStateKVM(t *testing.T) {
	if _, err := os.Stat("/dev/kvm"); err != nil {
		t.Skip("skipping co-located-fork inheritance proof: /dev/kvm not available (needs a KVM runner)")
	}
	snapDir := os.Getenv("MITOS_KVM_HUSK_SNAPSHOT_DIR")
	if snapDir == "" {
		t.Skip("skipping co-located-fork inheritance proof: set MITOS_KVM_HUSK_SNAPSHOT_DIR (the KVM CI sets it)")
	}
	for _, name := range []string{"mem", "vmstate"} {
		if _, err := os.Stat(filepath.Join(snapDir, name)); err != nil {
			t.Skipf("skipping co-located-fork inheritance proof: snapshot file %s not present: %v", name, err)
		}
	}
	// The bench template lays the source rootfs next to the snapshot subdir
	// (<data>/templates/<id>/rootfs.ext4, one level up from the snapshot dir). The
	// source stub needs its OWN CoW clone of it so its writes are captured by the
	// fork snapshot's frozen rootfs; without a real rootfs there is nothing to
	// inherit, so skip rather than assert.
	templateRootfs := filepath.Join(filepath.Dir(snapDir), "rootfs.ext4")
	if _, err := os.Stat(templateRootfs); err != nil {
		t.Skipf("skipping co-located-fork inheritance proof: template rootfs %s not present: %v", templateRootfs, err)
	}
	fcBin := os.Getenv("MITOS_KVM_FIRECRACKER")
	if fcBin == "" {
		fcBin = "/usr/local/bin/firecracker"
	}

	ctx := context.Background()

	// One --multi-vm source stub with a per-activation rootfs CoW so the source VM
	// writes its OWN clone (captured by the fork snapshot) and each co-located child
	// gets its own independent clone. AllowUnverified mirrors the CI snapshot's
	// unsigned manifest, exactly like the other husk KVM tests.
	src := New(firecracker.VMConfig{
		ID:             "husk-colo-src",
		FirecrackerBin: fcBin,
		WorkDir:        t.TempDir(),
		VcpuCount:      1,
		MemSizeMib:     256,
	}, Options{
		AllowUnverified:    true,
		ReadyTimeout:       30 * time.Second,
		MultiVM:            true,
		RootfsTemplatePath: templateRootfs,
		RootfsCoWDir:       t.TempDir(),
	})
	defer func() { _ = src.Close() }()

	// Bring up the source (default) VM from the template snapshot.
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

	// Write a UNIQUE marker file to the source rootfs and sync it onto the block
	// device so the fork snapshot's FROZEN rootfs carries it (the DISK signal). A
	// template boot never has this file.
	const markerPath = "/mitos-colo-marker.txt"
	marker := fmt.Sprintf("colo-inherit-%d", time.Now().UnixNano())
	if _, err := kvmExecOK(srcAgent, fmt.Sprintf("printf %s > %s && /bin/busybox sync", marker, markerPath)); err != nil {
		t.Fatalf("write+sync marker in source: %v", err)
	}

	// Let the source's guest clock advance well past the template's baked uptime, so
	// a child that restores the FORK memory reads a MUCH larger /proc/uptime than a
	// child that boots the template. Uptime lives only in guest RAM, so it is a clean
	// MEMORY-inheritance signal that no disk write can fake.
	time.Sleep(9 * time.Second)

	// Fork snapshot the source's default VM: pause -> mem+vmstate -> freeze the
	// source rootfs clone to <forkDir>/rootfs.ext4 -> resume. This is the exact
	// artifact the controller's fork-snapshot op produces in the source pod.
	forkDir := filepath.Join(t.TempDir(), "fork-snap")
	fres, err := src.ForkSnapshot(ctx, ForkSnapshotRequest{ForkID: "colo-child", SnapshotDir: forkDir})
	if err != nil || !fres.OK {
		t.Fatalf("source ForkSnapshot must succeed on KVM: err=%v res=%+v", err, fres)
	}
	for _, name := range []string{"mem", "vmstate", "rootfs.ext4"} {
		if _, err := os.Stat(filepath.Join(forkDir, name)); err != nil {
			t.Fatalf("fork snapshot did not write %s (needed for co-located inheritance): %v", name, err)
		}
	}

	// BUG-REPRO co-located child: the OLD wiring's request, activating from the
	// TEMPLATE snapshot with ForkSnapshot=false. It clones the TEMPLATE rootfs and
	// loads TEMPLATE memory, so it inherits nothing. Driving it here proves on the
	// SAME runner that the pre-fix wiring loses the parent's state.
	bugRes := src.SpawnVM(ctx, SpawnVMRequest{
		VMID:     "colo-bug",
		Activate: ActivateRequest{SnapshotDir: snapDir},
	})
	if !bugRes.OK {
		t.Fatalf("bug-repro spawn (template) must still boot a fresh VM: %+v", bugRes)
	}
	bugAgent, err := kvmConnectAgent(bugRes.VsockPath)
	if err != nil {
		t.Fatalf("connect bug-repro child agent: %v", err)
	}
	defer bugAgent.Close() //nolint:errcheck // best-effort teardown

	// FIXED co-located child: the request the fix sends, activating from the FORK
	// snapshot with ForkSnapshot=true. SpawnVM clones the FROZEN source rootfs at
	// forkDir/rootfs.ext4 and loads the fork mem+vmstate, so the child inherits the
	// parent's disk AND memory. This is the identical SpawnVM path the controller
	// drives (SpawnVMOnHusk -> Stub.SpawnVM -> activateInstance).
	fixRes := src.SpawnVM(ctx, SpawnVMRequest{
		VMID:     "colo-fix",
		Activate: ActivateRequest{SnapshotDir: forkDir, ForkSnapshot: true},
	})
	if !fixRes.OK {
		t.Fatalf("fixed co-located spawn from the fork snapshot must succeed: %+v", fixRes)
	}
	fixAgent, err := kvmConnectAgent(fixRes.VsockPath)
	if err != nil {
		t.Fatalf("connect fixed child agent: %v", err)
	}
	defer fixAgent.Close() //nolint:errcheck // best-effort teardown

	// DISK inheritance: the fixed child reads back the marker; the bug-repro child
	// does not (the file never existed on the template rootfs).
	readMarker := func(agent *guestgrpc.Client) string {
		out, _ := kvmExecOK(agent, fmt.Sprintf("cat %s 2>/dev/null || true", markerPath))
		return strings.TrimSpace(out)
	}
	if got := readMarker(bugAgent); got == marker {
		t.Fatalf("bug-repro child unexpectedly has the source marker %q; the template path should NOT inherit disk", marker)
	}
	if got := readMarker(fixAgent); got != marker {
		t.Fatalf("DISK inheritance FAILED: fixed co-located child read %q, want the source marker %q (frozen source rootfs not restored)", got, marker)
	}

	// MEMORY inheritance: the fixed child's uptime is far ahead of the bug-repro
	// child's, which sits at the template's baked uptime. The 9s the source ran
	// before the fork snapshot separates them; a 4s floor absorbs scheduling slack.
	bugUptime := kvmReadUptime(t, bugAgent)
	fixUptime := kvmReadUptime(t, fixAgent)
	if fixUptime <= bugUptime+4 {
		t.Fatalf("MEMORY inheritance FAILED: fixed child uptime %.2fs must be well ahead of the template-boot bug-repro child %.2fs (fork memory checkpoint not restored)", fixUptime, bugUptime)
	}
}

// kvmReadUptime reads the guest's /proc/uptime and returns the first field (seconds
// since the guest booted) as a float. It fails the test on a transport or parse
// error so a broken read is never silently treated as zero uptime.
func kvmReadUptime(t *testing.T, agent *guestgrpc.Client) float64 {
	t.Helper()
	out, err := kvmExecOK(agent, "cat /proc/uptime")
	if err != nil {
		t.Fatalf("read /proc/uptime: %v", err)
	}
	fields := strings.Fields(out)
	if len(fields) == 0 {
		t.Fatalf("empty /proc/uptime output %q", out)
	}
	v, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		t.Fatalf("parse uptime %q: %v", fields[0], err)
	}
	return v
}

// kvmConnectAgent dials the guest agent's gRPC service on the vsock UDS with a
// bounded retry.
func kvmConnectAgent(udsPath string) (*guestgrpc.Client, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client, err := guestgrpc.WaitReady(ctx, udsPath, 30*time.Second)
	if err != nil {
		return nil, fmt.Errorf("connect agent gRPC: %w", err)
	}
	return client, nil
}

// kvmExecOK runs a shell command in the guest via the gRPC Sandbox service and
// returns stdout, failing if the transport errors or the command exits nonzero.
func kvmExecOK(client *guestgrpc.Client, command string) (string, error) {
	stream, err := client.Sandbox.ExecStream(context.Background(), &sandboxv1.ExecStreamRequest{
		Command:        command,
		Cwd:            "/",
		TimeoutSeconds: 60,
	})
	if err != nil {
		return "", fmt.Errorf("exec stream: %w", err)
	}
	var stdout, stderr strings.Builder
	var exitCode int32
	for {
		msg, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", fmt.Errorf("recv exec frame: %w", err)
		}
		switch m := msg.Msg.(type) {
		case *sandboxv1.ExecResponse_Stdout:
			stdout.Write(m.Stdout)
		case *sandboxv1.ExecResponse_Stderr:
			stderr.Write(m.Stderr)
		case *sandboxv1.ExecResponse_Exit:
			exitCode = m.Exit.GetExitCode()
			if spawnErr := m.Exit.GetError(); spawnErr != "" {
				return "", fmt.Errorf("exec spawn error: %s", spawnErr)
			}
		}
	}
	if exitCode != 0 {
		return stdout.String(), fmt.Errorf("command %q exited %d: %s", command, exitCode, stderr.String())
	}
	return stdout.String(), nil
}
