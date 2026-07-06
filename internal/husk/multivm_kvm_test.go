package husk

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
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
		if err := s.prepareInstance(context.Background(), id); err != nil {
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
