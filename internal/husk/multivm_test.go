package husk

import (
	"context"
	"testing"

	"mitos.run/mitos/internal/firecracker"
)

// TestMultiVMFlagDefaultsOff proves the multi-VM execution mode is OFF by
// default: New(cfg, Options{}) leaves the stub on the single-VM path and
// allocates no scaffold. This is the guard that increment 1 of the
// multi-VM-per-pod work (#764) changes no runtime behavior; the scaffold is only
// reachable when a caller opts in, and no production caller does.
func TestMultiVMFlagDefaultsOff(t *testing.T) {
	s := New(firecracker.VMConfig{ID: "husk-test"}, Options{})
	if s.multiVM {
		t.Fatal("multiVM must default to false")
	}
	if s.instances != nil {
		t.Fatalf("single-VM path must not allocate the instances scaffold, got %v", s.instances)
	}
}

// TestMultiVMOptionOptsIn proves the flag threads through Options and populates
// the scaffold instances map (keyed by the single implicit vm id) only when a
// caller opts in. The map is the seam increment 2 of #764 migrates the single-VM
// state onto; nothing reads it on the runtime path today.
func TestMultiVMOptionOptsIn(t *testing.T) {
	s := New(firecracker.VMConfig{ID: "husk-test"}, Options{MultiVM: true})
	if !s.multiVM {
		t.Fatal("Options.MultiVM=true must set multiVM")
	}
	inst, ok := s.instances[defaultVMID]
	if !ok {
		t.Fatalf("expected a scaffold instance under %q, got %v", defaultVMID, s.instances)
	}
	if inst.state != StateNew {
		t.Fatalf("scaffold instance must start StateNew, got %s", inst.state)
	}
}

// TestNewVMInstanceIsCleanStateNew documents the scaffold contract increment 2
// of #764 builds on: a freshly constructed per-VM instance carries no VMM and no
// per-activation artifacts and starts in StateNew, so a later increment can key
// one such instance per fork. It reads every vmInstance field so the deferred
// runtime migration does not leave the scaffold fields dead.
func TestNewVMInstanceIsCleanStateNew(t *testing.T) {
	inst := newVMInstance()
	if inst.state != StateNew {
		t.Fatalf("state = %s, want new", inst.state)
	}
	if inst.vm != nil {
		t.Fatal("vm must be nil on a fresh instance")
	}
	if inst.generation != 0 {
		t.Fatalf("generation = %d, want 0", inst.generation)
	}
	if inst.prepareVerified {
		t.Fatal("prepareVerified must be false on a fresh instance")
	}
	if inst.rootfsClonePath != "" {
		t.Fatalf("rootfsClonePath = %q, want empty", inst.rootfsClonePath)
	}
	if inst.activeTap != "" {
		t.Fatalf("activeTap = %q, want empty", inst.activeTap)
	}
	if inst.dnsProxy != nil {
		t.Fatal("dnsProxy must be nil on a fresh instance")
	}
}

// TestSingleVMRoundtripUnchangedWithFlagOff drives the full lifecycle with the
// flag OFF and asserts the single-VM state machine is byte-for-byte the prior
// behavior: Prepare -> StateDormant, Activate -> StateActive, Close -> StateNew
// with the VMM torn down. This is the behavior-preservation proof for the
// default path that increment 1 must not disturb.
func TestSingleVMRoundtripUnchangedWithFlagOff(t *testing.T) {
	vm := &fakeVMM{}
	s := newTestStub(t, vm, readyOK)
	if s.multiVM {
		t.Fatal("newTestStub must be single-VM")
	}
	if err := s.Prepare(context.Background()); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if s.State() != StateDormant {
		t.Fatalf("after Prepare state = %s, want dormant", s.State())
	}
	res, err := s.Activate(context.Background(), ActivateRequest{SnapshotDir: "/snap"})
	if err != nil || !res.OK {
		t.Fatalf("Activate: err=%v ok=%v", err, res.OK)
	}
	if s.State() != StateActive {
		t.Fatalf("after Activate state = %s, want active", s.State())
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if s.State() != StateNew {
		t.Fatalf("after Close state = %s, want new", s.State())
	}
	if !vm.closed {
		t.Fatal("Close must tear the VMM down")
	}
}
