package husk

import (
	"context"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"mitos.run/mitos/internal/firecracker"
	"mitos.run/mitos/internal/fork"
)

// fakeLiveCowParent stands in for the armed parent-side WP handle: it satisfies
// BOTH fork.ChildImportProvider (so SetLiveCowParent accepts it) AND the
// liveCowForkFreezer seam forkSnapshotInstance calls at the fork point. Freeze
// records that it ran (the ~9us m2 write-protect the vmstate-only path substitutes
// for the 364ms mem write) and returns a canned duration/err.
type fakeLiveCowParent struct {
	freezeCalls int32
	freezeErr   error
	freezeDur   time.Duration
}

func (p *fakeLiveCowParent) Freeze() (time.Duration, error) {
	atomic.AddInt32(&p.freezeCalls, 1)
	return p.freezeDur, p.freezeErr
}

func (p *fakeLiveCowParent) ChildImport(string) (fork.ChildMemfdImport, error) {
	return fork.ChildMemfdImport{}, nil
}

// liveCowArmedStub builds a multi-VM stub with the live-cow flag ON, then
// Prepare+Activate its default VM so a fork snapshot can run, and arms the given
// parent freezer. childImport sets Options.LiveCowChildImport: true opts the fork
// onto the vmstate-only (no-mem) capture (the child boots from the shared memfd),
// false keeps the disk-restorable Full path even for an armed source (the shipped
// production posture, where no child-side memfd-import Firecracker patch exists).
// It returns the stub and the default VM's fake handle.
func liveCowArmedStub(t *testing.T, parent fork.ChildImportProvider, childImport bool) (*Stub, *fakeVMM) {
	t.Helper()
	vms := map[string]*fakeVMM{}
	start := func(cfg firecracker.VMConfig) (vmm, error) {
		vm := &fakeVMM{}
		vms[cfg.ID] = vm
		return vm, nil
	}
	s := New(firecracker.VMConfig{ID: "husk-test"}, Options{
		Start:              start,
		Ready:              readyOK,
		Notify:             (&fakeNotifier{}).notify,
		Verify:             verifyOK,
		MultiVM:            true,
		LiveCowFork:        true,
		LiveCowChildImport: childImport,
	})
	if parent != nil {
		s.SetLiveCowParent(parent)
	}
	ctx := context.Background()
	if err := s.Prepare(ctx); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if res, err := s.Activate(ctx, ActivateRequest{SnapshotDir: "/snap"}); err != nil || !res.OK {
		t.Fatalf("Activate: err=%v res=%+v", err, res)
	}
	return s, s.instances[defaultVMID].vm.(*fakeVMM)
}

// TestForkSnapshotLiveCowCapturesVMStateOnly is the item-1-of-#832 unit gate: with
// the live-cow path armed, a fork snapshot FREEZES the source guest and captures
// ONLY the vmstate, writing NO mem file (the 364ms guest-RAM copy is skipped). It
// asserts the vmstate-only VMM call ran, the Full CreateSnapshot(mem, vmstate) did
// NOT, no mem path was passed, and the paused-window stage timing carries both a
// `freeze` and a `create_snapshot` stage.
func TestForkSnapshotLiveCowCapturesVMStateOnly(t *testing.T) {
	parent := &fakeLiveCowParent{freezeDur: 9 * time.Microsecond}
	s, f := liveCowArmedStub(t, parent, true) // child import ON: vmstate-only capture

	dir := t.TempDir()
	res, err := s.ForkSnapshot(context.Background(), ForkSnapshotRequest{ForkID: "fork-1", SnapshotDir: dir})
	if err != nil || !res.OK {
		t.Fatalf("ForkSnapshot (live-cow): err=%v res=%+v", err, res)
	}

	if atomic.LoadInt32(&parent.freezeCalls) != 1 {
		t.Errorf("source guest must be frozen exactly once on the live-cow path, freezeCalls=%d", parent.freezeCalls)
	}
	if !f.snapVMStateOnly {
		t.Errorf("live-cow fork must take the vmstate-only capture")
	}
	if f.snapMem != "" {
		t.Errorf("live-cow fork must write NO mem file, but a Full snapshot wrote mem=%q", f.snapMem)
	}
	if f.snapState != filepath.Join(dir, "vmstate") {
		t.Errorf("vmstate written to wrong path: %q", f.snapState)
	}
	for _, stage := range []string{"pause", "freeze", "create_snapshot", "resume"} {
		if _, ok := res.Stages[stage]; !ok {
			t.Errorf("live-cow fork result missing stage %q; got %v", stage, res.Stages)
		}
	}
	// The source must be resumed after the checkpoint (never left frozen).
	if !f.resumed {
		t.Errorf("source VM must be resumed after a live-cow fork snapshot")
	}
	if s.State() != StateNew {
		t.Errorf("single-VM state stays New under multi-vm, got %s", s.State())
	}
}

// TestForkSnapshotFallsBackToFullWhenNoArmedParent proves the fail-safe: with the
// live-cow FLAG on but NO armed parent (the state until parent-arm wiring lands),
// a fork takes the Full CreateSnapshot(mem, vmstate) path byte-for-byte, so a fork
// never breaks and the mem file is still written for the disk restore.
func TestForkSnapshotFallsBackToFullWhenNoArmedParent(t *testing.T) {
	s, f := liveCowArmedStub(t, nil, true) // flag on, parent NOT armed

	dir := t.TempDir()
	res, err := s.ForkSnapshot(context.Background(), ForkSnapshotRequest{ForkID: "fork-1", SnapshotDir: dir})
	if err != nil || !res.OK {
		t.Fatalf("ForkSnapshot (fallback): err=%v res=%+v", err, res)
	}
	if f.snapVMStateOnly {
		t.Errorf("with no armed parent the fork must NOT take the vmstate-only path")
	}
	if f.snapMem != filepath.Join(dir, "mem") {
		t.Errorf("fallback fork must write the Full mem file, got mem=%q", f.snapMem)
	}
	if _, ok := res.Stages["freeze"]; ok {
		t.Errorf("fallback fork must not record a freeze stage; got %v", res.Stages)
	}
}

// TestForkSnapshotArmedSourceWritesMemWhenChildImportOff is the prod-hang gate
// (v1.32.2 canary: co-located fork children stuck Restoring until the client 120s
// deadline). Even with the source ARMED (a freezer available), the fork must keep
// writing the disk `mem` file UNLESS child import is enabled, because the shipped
// Firecracker patches the SOURCE side only: a co-located child restores from the
// DISK fork snapshot, and a vmstate-only (no-mem) snapshot leaves it nothing to
// restore. So arm alone must NOT skip the mem file, or the fork hangs. It asserts
// the Full path ran (mem written), the source was NOT frozen, and no `freeze`
// stage was recorded, so a re-enabled live-cow source's forks stay restorable.
func TestForkSnapshotArmedSourceWritesMemWhenChildImportOff(t *testing.T) {
	parent := &fakeLiveCowParent{freezeDur: 9 * time.Microsecond}
	s, f := liveCowArmedStub(t, parent, false) // ARMED, but child import OFF (prod posture)

	dir := t.TempDir()
	res, err := s.ForkSnapshot(context.Background(), ForkSnapshotRequest{ForkID: "fork-1", SnapshotDir: dir})
	if err != nil || !res.OK {
		t.Fatalf("ForkSnapshot (armed, child-import off): err=%v res=%+v", err, res)
	}
	if atomic.LoadInt32(&parent.freezeCalls) != 0 {
		t.Errorf("an armed source must NOT be frozen when child import is off (the Full path skips the freeze), freezeCalls=%d", parent.freezeCalls)
	}
	if f.snapVMStateOnly {
		t.Errorf("armed source with child import off must NOT take the vmstate-only path (the co-located child restores from disk)")
	}
	if f.snapMem != filepath.Join(dir, "mem") {
		t.Errorf("armed source with child import off must write the disk mem file so the co-located child is restorable, got mem=%q", f.snapMem)
	}
	if _, ok := res.Stages["freeze"]; ok {
		t.Errorf("armed source with child import off must not record a freeze stage; got %v", res.Stages)
	}
	if !f.resumed {
		t.Errorf("source VM must be resumed after the fork snapshot")
	}
}

// TestForkSnapshotLiveCowResumesSourceOnFreezeError proves fail-closed: a freeze
// failure resumes the source (never leaves a tenant's live sandbox frozen) and
// fails the fork, without ever writing a snapshot.
func TestForkSnapshotLiveCowResumesSourceOnFreezeError(t *testing.T) {
	parent := &fakeLiveCowParent{freezeErr: errSnap}
	s, f := liveCowArmedStub(t, parent, true) // child import ON: exercises the freeze path

	dir := t.TempDir()
	res, err := s.ForkSnapshot(context.Background(), ForkSnapshotRequest{ForkID: "fork-1", SnapshotDir: dir})
	if err == nil || res.OK {
		t.Fatalf("ForkSnapshot must fail closed on a freeze error, got err=%v res=%+v", err, res)
	}
	if f.snapVMStateOnly || f.snapMem != "" {
		t.Errorf("no snapshot must be written when the freeze fails")
	}
	if !f.resumed {
		t.Errorf("source VM must be resumed after a freeze failure (never left frozen)")
	}
}
