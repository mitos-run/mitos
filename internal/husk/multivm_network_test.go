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
		// ip tuntap add <tap> mode tap
		if len(c.argv) >= 4 && c.argv[0] == "ip" && c.argv[1] == "tuntap" && c.argv[2] == "add" {
			switch c.argv[3] {
			case wantDefaultTap:
				madeDefault++
			case wantSecondTap:
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
