package husk

import (
	"context"
	"path/filepath"
	"testing"

	"mitos.run/mitos/internal/cas"
	"mitos.run/mitos/internal/workspace"
)

// TestMultiVMForkSnapshotFromDefaultVM is the L1.8 prod-canary regression: with
// --multi-vm ON, the CLAIM path (Activate) advances the DEFAULT instance's state
// to Active via activateInstance, NOT the single-VM s.state, which stays
// StateNew. Before the fix the public ForkSnapshot still gated on s.state and so
// refused EVERY fork of a multi-vm source with "fork-snapshot in state new: must
// be active", timing out the hosted fork loop. After the fix ForkSnapshot routes
// through the default instance under --multi-vm and reads the state Activate set,
// so a fork of a multi-vm stub's default VM SUCCEEDS.
//
// This test FAILS on origin/main (ForkSnapshot has no multi-VM branch) and PASSES
// once ForkSnapshot routes through forkSnapshotInstance(defaultVMID).
func TestMultiVMForkSnapshotFromDefaultVM(t *testing.T) {
	vms := map[string]*fakeVMM{}
	s := newMultiVMTestStub(t, vms)

	ctx := context.Background()
	// Drive the PUBLIC lifecycle so the multi-VM dispatch (not the per-instance
	// helpers directly) is exercised: Prepare -> Activate route to the default
	// instance, leaving s.state StateNew but inst.state StateActive.
	if err := s.Prepare(ctx); err != nil {
		t.Fatalf("Prepare (multi-vm default): %v", err)
	}
	if res, err := s.Activate(ctx, ActivateRequest{SnapshotDir: "/snap"}); err != nil || !res.OK {
		t.Fatalf("Activate (multi-vm default): err=%v res=%+v", err, res)
	}
	// The single-VM state field is deliberately still StateNew: the whole point of
	// the bug is that ForkSnapshot must NOT read it under multi-vm.
	if s.state != StateNew {
		t.Fatalf("precondition: single-VM s.state must stay StateNew under multi-vm, got %s", s.state)
	}
	if got := s.instances[defaultVMID].state; got != StateActive {
		t.Fatalf("precondition: default instance must be active, got %s", got)
	}

	dir := t.TempDir()
	res, err := s.ForkSnapshot(ctx, ForkSnapshotRequest{ForkID: "fork-1", SnapshotDir: dir})
	if err != nil {
		t.Fatalf("ForkSnapshot on a multi-vm stub's default VM must succeed, got err: %v", err)
	}
	if !res.OK {
		t.Fatalf("ForkSnapshot on a multi-vm stub's default VM must be OK, got: %+v", res)
	}

	// The DEFAULT instance's VM (not the single-VM s.vm, which is nil under
	// multi-vm) must have been paused for the checkpoint and resumed afterward.
	defVM := vms["husk-test"]
	if defVM == nil {
		t.Fatalf("expected the default VM fake under derived id husk-test, got keys %v", vms)
	}
	if !defVM.paused {
		t.Fatalf("the default instance's VM was not paused for the fork snapshot")
	}
	if !defVM.resumed {
		t.Fatalf("the default instance's VM was not resumed after the fork snapshot")
	}
	if defVM.snapMem != filepath.Join(dir, "mem") || defVM.snapState != filepath.Join(dir, "vmstate") {
		t.Fatalf("fork snapshot written to wrong paths: mem=%s state=%s", defVM.snapMem, defVM.snapState)
	}
	// The default instance stays Active throughout: it still owns its VM.
	if got := s.instances[defaultVMID].state; got != StateActive {
		t.Fatalf("default instance must remain active after fork snapshot, got %s", got)
	}
}

// TestMultiVMForkSnapshotRequiresActiveDefaultInstance proves the routed
// ForkSnapshot still fails closed under --multi-vm when the default instance is
// NOT active (only prepared): the gate now reads inst.state, so a dormant default
// VM is refused with a must-be-active error and its VM is never paused.
func TestMultiVMForkSnapshotRequiresActiveDefaultInstance(t *testing.T) {
	vms := map[string]*fakeVMM{}
	s := newMultiVMTestStub(t, vms)

	ctx := context.Background()
	if err := s.Prepare(ctx); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	// No Activate: the default instance is StateDormant.
	res, err := s.ForkSnapshot(ctx, ForkSnapshotRequest{ForkID: "f", SnapshotDir: t.TempDir()})
	if err == nil || res.OK {
		t.Fatalf("fork snapshot of a non-active default VM must fail closed: err=%v res=%+v", err, res)
	}
	if vms["husk-test"].paused {
		t.Fatalf("a refused fork snapshot must not pause the default VM")
	}
}

// TestMultiVMWorkspaceRoutesToDefaultInstance proves the same class of fix for the
// workspace ops: under --multi-vm, DehydrateWorkspace and HydrateWorkspace route
// through the DEFAULT instance's state and VM instead of the single-VM s.state
// (StateNew) / s.vm (nil). Before the fix both refused with "state new: must be
// active" on a multi-vm pod; after the fix they run against the active default VM.
func TestMultiVMWorkspaceRoutesToDefaultInstance(t *testing.T) {
	store, err := cas.New(t.TempDir())
	if err != nil {
		t.Fatalf("cas.New: %v", err)
	}
	src := &fakeWorkspaceAgent{tar: tarOf(t, map[string]string{"main.go": "package main"})}
	// A stub in multi-VM mode whose DEFAULT instance is active with a fake VM, but
	// whose single-VM s.state / s.vm are the zero value (StateNew / nil), exactly as
	// New(Options{MultiVM:true}) leaves them after a routed Activate.
	s := &Stub{
		multiVM:      true,
		vsockRelPath: "v.sock",
		casStore:     store,
		wsTransport:  func(string) (workspace.VsockTransport, error) { return src, nil },
		instances:    map[vmID]*vmInstance{defaultVMID: {state: StateActive, vm: &fakeVMM{}}},
	}

	dres, err := s.DehydrateWorkspace(context.Background(), DehydrateWorkspaceRequest{})
	if err != nil || !dres.OK {
		t.Fatalf("DehydrateWorkspace on a multi-vm stub's default VM must succeed: err=%v res=%+v", err, dres)
	}
	d := cas.Digest(dres.ManifestDigest)
	if err := d.Validate(); err != nil {
		t.Fatalf("dehydrate produced an invalid manifest digest: %v", err)
	}

	// Hydrate the captured manifest back into a fresh multi-vm stub's default VM.
	dst := &fakeWorkspaceAgent{}
	s2 := &Stub{
		multiVM:      true,
		vsockRelPath: "v.sock",
		casStore:     store,
		wsTransport:  func(string) (workspace.VsockTransport, error) { return dst, nil },
		instances:    map[vmID]*vmInstance{defaultVMID: {state: StateActive, vm: &fakeVMM{}}},
	}
	hres, err := s2.HydrateWorkspace(context.Background(), HydrateWorkspaceRequest{ManifestDigest: dres.ManifestDigest})
	if err != nil || !hres.OK {
		t.Fatalf("HydrateWorkspace on a multi-vm stub's default VM must succeed: err=%v res=%+v", err, hres)
	}
	if dst.untarPath != workspace.WorkspacePath {
		t.Fatalf("hydrate must untar into %s, got %s", workspace.WorkspacePath, dst.untarPath)
	}
}
