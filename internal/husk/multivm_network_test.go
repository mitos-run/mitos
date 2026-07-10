package husk

import (
	"context"
	"strings"
	"testing"

	"mitos.run/mitos/internal/firecracker"
	"mitos.run/mitos/internal/netconf"
	"mitos.run/mitos/internal/vsock"
)

// baseNet is the pod's primary VM network the multi-VM derivation starts from.
func baseNet() *vsock.NotifyForkedNetwork {
	return &vsock.NotifyForkedNetwork{
		GuestIP:    "10.200.0.2",
		GatewayIP:  "10.200.0.1",
		PrefixLen:  30,
		GuestMAC:   "02:00:00:00:00:01",
		ResolverIP: "169.254.1.1",
	}
}

// TestDeriveVMNetworkDistinctPerVMID proves the per-VM network derivation the
// L1.4 activate path keys on: the DEFAULT (primary) VM keeps the base guest IP,
// gateway, and MAC, while every OTHER vmID gets its OWN distinct guest IP,
// gateway, MAC, and (since the tap derives from the guest IP) tap, so two VMs in
// one pod netns can never collide or cross each other's egress. It also proves
// the derivation is a pure, deterministic function of the vmID, never mutates the
// caller's base, and clears ResolverIP (DNS-proxy fan-out is deferred to L1.4b).
func TestDeriveVMNetworkDistinctPerVMID(t *testing.T) {
	base := baseNet()

	def := deriveVMNetwork(defaultVMID, base)
	if def.GuestIP != base.GuestIP || def.GatewayIP != base.GatewayIP || def.GuestMAC != base.GuestMAC {
		t.Fatalf("default VM must keep the base link, got %+v", def)
	}
	if def.ResolverIP != "" {
		t.Errorf("multi-VM must clear ResolverIP (DNS fan-out deferred), got %q", def.ResolverIP)
	}

	a := deriveVMNetwork("vm-a", base)
	b := deriveVMNetwork("vm-b", base)

	// Two secondaries differ from each other AND from the primary on every field
	// that separates their datapaths.
	for _, f := range []struct {
		name         string
		av, bv, defv string
	}{
		{"guest IP", a.GuestIP, b.GuestIP, def.GuestIP},
		{"gateway IP", a.GatewayIP, b.GatewayIP, def.GatewayIP},
		{"MAC", a.GuestMAC, b.GuestMAC, def.GuestMAC},
	} {
		if f.av == f.bv {
			t.Errorf("%s collides between vm-a and vm-b: %q", f.name, f.av)
		}
		if f.av == f.defv || f.bv == f.defv {
			t.Errorf("%s of a secondary collides with the primary: %q/%q vs %q", f.name, f.av, f.bv, f.defv)
		}
	}
	if netconf.DeriveTapName(a.GuestIP) == netconf.DeriveTapName(b.GuestIP) {
		t.Errorf("two secondaries derive the same tap")
	}
	if netconf.DeriveTapName(a.GuestIP) == netconf.DeriveTapName(def.GuestIP) {
		t.Errorf("a secondary derives the primary's tap")
	}

	// Deterministic and non-mutating.
	if a2 := deriveVMNetwork("vm-a", base); a2.GuestIP != a.GuestIP || a2.GuestMAC != a.GuestMAC {
		t.Errorf("derivation not deterministic: %+v vs %+v", a, a2)
	}
	if base.GuestIP != "10.200.0.2" || base.ResolverIP != "169.254.1.1" {
		t.Errorf("derivation mutated the caller's base network: %+v", base)
	}

	// A nil base (the unit path programs no networking) is returned unchanged.
	if deriveVMNetwork("vm-a", nil) != nil {
		t.Errorf("nil base must derive nil")
	}
}

// TestDeriveVMConfigDistinctSocketsPerVMID proves the per-VM Firecracker identity
// the real second VM relies on: two vmIDs derive distinct process IDs, workdirs,
// and API socket paths nested under the pod workdir, while the default VM keeps
// the pod's base config. This is what lets two Firecracker processes coexist in
// one pod without colliding on the single fixed socket the single-VM path uses.
func TestDeriveVMConfigDistinctSocketsPerVMID(t *testing.T) {
	s := New(firecracker.VMConfig{ID: "husk-test", WorkDir: "/run/husk"}, Options{MultiVM: true})

	def := s.deriveVMConfig(defaultVMID)
	if def.ID != "husk-test" || def.WorkDir != "/run/husk" {
		t.Fatalf("default VM must keep the base config, got %+v", def)
	}

	a := s.deriveVMConfig("vm-a")
	b := s.deriveVMConfig("vm-b")
	if a.ID == b.ID || a.WorkDir == b.WorkDir || a.SocketPath == b.SocketPath {
		t.Fatalf("two vmIDs must derive distinct id/workdir/socket: a=%+v b=%+v", a, b)
	}
	if a.ID == def.ID || a.SocketPath == def.SocketPath {
		t.Fatalf("a secondary must differ from the primary: a=%+v def=%+v", a, def)
	}
	if !strings.HasPrefix(a.WorkDir, "/run/husk/") || !strings.HasPrefix(a.SocketPath, "/run/husk/") {
		t.Errorf("secondary paths must nest under the pod workdir, got %+v", a)
	}
}

// TestMultiVMActivateProgramsDistinctTapPerVMID drives the REAL activateInstance
// networking (with an injected recording runner, no KVM) and proves each vmID's
// VM is brought up on its OWN tap: the primary on the base guest IP's tap, the
// secondary on its derived tap, the two taps distinct, each VM's baked NIC bound
// to its OWN tap, and the derived per-VM network delivered to each guest. This is
// the deterministic proof that two VMs in one pod cannot cross egress; only the
// KVM CI proves the same wiring against a real second Firecracker process.
func TestMultiVMActivateProgramsDistinctTapPerVMID(t *testing.T) {
	rr := &recordingRunner{}
	n := &fakeNotifier{}
	vms := map[string]*fakeVMM{}
	s := New(firecracker.VMConfig{ID: "husk-test", WorkDir: t.TempDir()}, Options{
		Start:   func(cfg firecracker.VMConfig) (vmm, error) { vm := &fakeVMM{}; vms[cfg.ID] = vm; return vm, nil },
		Ready:   readyOK,
		Notify:  n.notify,
		Verify:  verifyOK,
		MultiVM: true,
	})
	s.netRunner = rr.run

	const second vmID = "vm-2"
	for _, id := range []vmID{defaultVMID, second} {
		if err := s.prepareInstance(context.Background(), id, "", nil); err != nil {
			t.Fatalf("prepareInstance(%s): %v", id, err)
		}
		req := ActivateRequest{
			SnapshotDir: "/snap",
			Egress:      "deny",
			Allow:       []string{"10.0.0.5:5432"},
			Network:     baseNet(),
			VMID:        string(id),
		}
		if id == defaultVMID {
			req.VMID = ""
		}
		res, err := s.activateInstance(context.Background(), id, req)
		if err != nil || !res.OK {
			t.Fatalf("activateInstance(%s): err=%v ok=%v", id, err, res.OK)
		}
	}

	wantDefaultTap := netconf.DeriveTapName(baseNet().GuestIP)
	wantSecondTap := netconf.DeriveTapName(deriveVMNetwork(second, baseNet()).GuestIP)
	if wantDefaultTap == wantSecondTap {
		t.Fatalf("derivation gave the two VMs the same tap %q", wantDefaultTap)
	}

	// Each VM's baked NIC is bound to its OWN derived tap.
	if got := vms["husk-test"].gotOverr; len(got) != 1 || got[0].HostDevName != wantDefaultTap {
		t.Errorf("primary NIC not bound to its tap %q: %+v", wantDefaultTap, got)
	}
	if got := vms["husk-test-vm-2"].gotOverr; len(got) != 1 || got[0].HostDevName != wantSecondTap {
		t.Errorf("secondary NIC not bound to its tap %q: %+v", wantSecondTap, got)
	}
	if inst := s.instances["vm-2"]; inst.activeTap != wantSecondTap {
		t.Errorf("secondary activeTap = %q, want %q", inst.activeTap, wantSecondTap)
	}

	// Both taps were actually created, and the default-deny metadata block was
	// applied for each VM (one egress chain per instance).
	var madeDefault, madeSecond, metadataBlocks int
	for _, c := range rr.calls {
		// The tap is created by an `ip -batch -` line: `tuntap add <tap> mode tap`
		// (the per-command execs are collapsed into one batched ip process).
		if len(c.argv) >= 2 && c.argv[0] == "ip" && c.argv[1] == "-batch" {
			if strings.Contains(c.stdin, "tuntap add "+wantDefaultTap+" mode tap") {
				madeDefault++
			}
			if strings.Contains(c.stdin, "tuntap add "+wantSecondTap+" mode tap") {
				madeSecond++
			}
		}
		if strings.Contains(c.stdin, "ip daddr 169.254.169.254 drop") {
			metadataBlocks++
		}
	}
	if madeDefault == 0 || madeSecond == 0 {
		t.Errorf("expected both taps created, got default=%d second=%d", madeDefault, madeSecond)
	}
	if metadataBlocks < 2 {
		t.Errorf("expected a per-VM egress chain with the metadata block for each VM, got %d", metadataBlocks)
	}

	// The guest was handed its OWN derived network (distinct guest IP), never the
	// pod base for both; and ResolverIP is cleared (DNS fan-out deferred to L1.4b).
	if len(n.gotReq) != 2 {
		t.Fatalf("expected 2 notify calls, got %d", len(n.gotReq))
	}
	seen := map[string]bool{}
	for _, r := range n.gotReq {
		if r.Network == nil {
			t.Fatal("notify must carry the per-VM network")
		}
		if r.Network.ResolverIP != "" {
			t.Errorf("notify network must clear ResolverIP, got %q", r.Network.ResolverIP)
		}
		seen[r.Network.GuestIP] = true
	}
	if len(seen) != 2 {
		t.Errorf("the two VMs must be handed distinct guest IPs, got %v", seen)
	}
}

// --- prepare-time egress link ------------------------------------------------------

// countIPBatch counts the ip -batch (tap create) invocations.
func countIPBatch(calls []recordedCall) int {
	n := 0
	for _, c := range calls {
		if len(c.argv) > 0 && strings.Contains(c.argv[0], "ip") && strings.Contains(strings.Join(c.argv, " "), "-batch") {
			n++
		}
	}
	return n
}

func countNft(calls []recordedCall) int {
	n := 0
	for _, c := range calls {
		if len(c.argv) > 0 && strings.Contains(c.argv[0], "nft") {
			n++
		}
	}
	return n
}

// newPreparedLinkStub builds a multi-VM stub that brings its default VM's tap up while
// dormant, over a recording net runner.
func newPreparedLinkStub(t *testing.T, rr *recordingRunner) *Stub {
	t.Helper()
	start := func(_ firecracker.VMConfig) (vmm, error) { return &fakeVMM{}, nil }
	s := New(firecracker.VMConfig{ID: "husk-test"}, Options{
		Start:             start,
		Ready:             readyOK,
		Notify:            (&fakeNotifier{}).notify,
		Verify:            verifyOK,
		MultiVM:           true,
		PrepareEgressLink: true,
		InPodGuestIP:      "10.200.0.2",
		InPodGatewayIP:    "10.200.0.1",
	})
	s.SetNetRunner(rr.run)
	return s
}

// TestPrepareBringsTheTapUpDormantAndActivateOnlyInstallsThePolicy is the point of the
// split. A pod that opted in pays the tap create while it is DORMANT, so the claim's
// egress_filter stage is one atomic nft transaction and nothing else. The dormant tap
// carries a default-deny policy, so the VM behind it is never unfiltered.
func TestPrepareBringsTheTapUpDormantAndActivateOnlyInstallsThePolicy(t *testing.T) {
	var rr recordingRunner
	s := newPreparedLinkStub(t, &rr)
	ctx := context.Background()

	if err := s.Prepare(ctx); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if got := countIPBatch(rr.calls); got != 1 {
		t.Errorf("Prepare ran %d tap creates, want exactly 1", got)
	}
	if got := countNft(rr.calls); got != 1 {
		t.Errorf("Prepare installed %d policies, want exactly 1 (the dormant default-deny)", got)
	}
	tap := netconf.DeriveTapName("10.200.0.2")
	if !strings.Contains(strings.Join(rr.calls[0].argv, " "), tap) {
		t.Errorf("Prepare did not touch the pod's tap %q: %v", tap, rr.calls[0].argv)
	}

	prepared := len(rr.calls)
	if _, err := s.Activate(ctx, ActivateRequest{SnapshotDir: "/snap", Network: baseNet()}); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	claim := rr.calls[prepared:]
	if got := countIPBatch(claim); got != 0 {
		t.Errorf("the claim re-created the tap %d times; the dormant pod already brought it up", got)
	}
	if got := countNft(claim); got != 1 {
		t.Errorf("the claim ran %d nft transactions, want exactly 1 (replace the policy): %v", got, claim)
	}
	if len(claim) != 1 {
		t.Errorf("the claim ran %d commands, want exactly 1: %v", len(claim), claim)
	}
}

// TestPrepareTimeLinkIsOptIn: a stub that did not opt in must behave byte-for-byte as
// before, doing all of it at activate.
func TestPrepareTimeLinkIsOptIn(t *testing.T) {
	var rr recordingRunner
	start := func(_ firecracker.VMConfig) (vmm, error) { return &fakeVMM{}, nil }
	s := New(firecracker.VMConfig{ID: "husk-test"}, Options{
		Start: start, Ready: readyOK, Notify: (&fakeNotifier{}).notify, Verify: verifyOK,
		MultiVM: true,
	})
	s.SetNetRunner(rr.run)
	ctx := context.Background()

	if err := s.Prepare(ctx); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if len(rr.calls) != 0 {
		t.Fatalf("Prepare touched the network without opting in: %v", rr.calls)
	}
	if _, err := s.Activate(ctx, ActivateRequest{SnapshotDir: "/snap", Network: baseNet()}); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if got := countIPBatch(rr.calls); got != 1 {
		t.Errorf("activate ran %d tap creates, want 1", got)
	}
	if got := countNft(rr.calls); got != 1 {
		t.Errorf("activate ran %d nft transactions, want 1", got)
	}
}

// TestAFailedClaimPolicyRebuildsTheLinkOnRetry: applyEgressPolicy fails closed by
// tearing the tap down, so the prepared marker must be cleared. Otherwise the retry
// would install a policy on a tap that no longer exists and the pod would be poisoned.
func TestAFailedClaimPolicyRebuildsTheLinkOnRetry(t *testing.T) {
	var rr recordingRunner
	s := newPreparedLinkStub(t, &rr)
	ctx := context.Background()
	if err := s.Prepare(ctx); err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	inst := s.instanceFor(defaultVMID, false)
	if inst == nil || inst.preparedLinkTap == "" {
		t.Fatal("Prepare did not record the prepared tap")
	}

	// A claim whose policy is rejected: the CIDR list cannot be parsed.
	if _, err := s.Activate(ctx, ActivateRequest{SnapshotDir: "/snap", Network: baseNet(), AllowCIDRs: []string{"not-a-cidr"}}); err == nil {
		t.Fatal("Activate accepted a malformed CIDR allowlist")
	}
	if inst.preparedLinkTap != "" {
		t.Error("a failed claim left the prepared-link marker set; the retry would skip rebuilding a torn-down tap")
	}
	if inst.activeTap != "" {
		t.Error("a failed claim left activeTap set for a tap that was torn down")
	}
}
