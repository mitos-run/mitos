package husk

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// activeStubWithFake returns a Stub already in StateActive holding the given fake
// vmm, so ForkSnapshot can be exercised without a real Activate. It uses the same
// fake vmm type the stub_test.go uses.
func activeStubWithFake(f *fakeVMM) *Stub {
	return &Stub{state: StateActive, vm: f}
}

func TestForkSnapshotPausesSnapshotsResumes(t *testing.T) {
	f := &fakeVMM{}
	s := activeStubWithFake(f)

	dir := t.TempDir()
	res, err := s.ForkSnapshot(context.Background(), ForkSnapshotRequest{
		ForkID:      "fork-1",
		SnapshotDir: dir,
		PauseSource: false,
	})
	if err != nil {
		t.Fatalf("ForkSnapshot: %v", err)
	}
	if !res.OK {
		t.Fatalf("ForkSnapshot not OK: %+v", res)
	}
	if !f.paused {
		t.Fatalf("source VM was not paused before snapshot")
	}
	if !f.resumed {
		t.Fatalf("source VM was not resumed after snapshot (PauseSource=false)")
	}
	if f.snapMem != filepath.Join(dir, "mem") || f.snapState != filepath.Join(dir, "vmstate") {
		t.Fatalf("snapshot written to wrong paths: mem=%s state=%s", f.snapMem, f.snapState)
	}
	if s.State() != StateActive {
		t.Fatalf("stub must remain active after fork snapshot, got %s", s.State())
	}
}

// TestForkSnapshotResumesSourceOnHostedPath is the production-confirmed bug fix
// (v1.24.1): the hosted SDK POSTs /v1/sandboxes/{id}/fork with pause_source=true
// to capture a quiescent checkpoint, but the source must still be RUNNING when
// the fork snapshot returns. Leaving it paused makes a post-fork exec against the
// SOURCE (the fork-the-winner-and-continue loop) time out at 30s. The pause is
// only the brief quiescence CreateSnapshot requires; it is always followed by a
// resume, so pause_source no longer leaves the source stopped.
func TestForkSnapshotResumesSourceOnHostedPath(t *testing.T) {
	f := &fakeVMM{}
	s := activeStubWithFake(f)
	res, err := s.ForkSnapshot(context.Background(), ForkSnapshotRequest{
		ForkID:      "fork-1",
		SnapshotDir: t.TempDir(),
		PauseSource: true,
	})
	if err != nil || !res.OK {
		t.Fatalf("ForkSnapshot: err=%v res=%+v", err, res)
	}
	if !f.paused {
		t.Fatalf("source VM was not paused for the checkpoint")
	}
	if !f.resumed {
		t.Fatalf("source VM must be RESUMED after the fork snapshot even with pause_source=true; leaving it paused breaks a post-fork exec against the source")
	}
}

// TestForkSnapshotFreezesRootfsInsidePausedWindow proves the source resume is
// safe: with a per-activation rootfs clone present (the real hosted path), the
// source's rootfs is frozen as a point-in-time copy INSIDE the paused window,
// paired with mem+vmstate, BEFORE the source resumes. Child husk pods clone from
// THIS frozen copy, so a resumed source that keeps writing its live disk can
// never drift a child's rootfs out of sync with the memory checkpoint.
func TestForkSnapshotFreezesRootfsInsidePausedWindow(t *testing.T) {
	f := &fakeVMM{}
	dir := t.TempDir()
	cloneDir := t.TempDir()
	clonePath := filepath.Join(cloneDir, "rootfs.ext4")
	if err := os.WriteFile(clonePath, []byte("live-source-disk"), 0o644); err != nil {
		t.Fatal(err)
	}
	var gotSrc, gotDst string
	s := &Stub{state: StateActive, vm: f, rootfsClonePath: clonePath}
	s.reflink = func(src, dst string) error {
		gotSrc, gotDst = src, dst
		f.mu.Lock()
		f.callOrder = append(f.callOrder, "reflink")
		f.mu.Unlock()
		return nil
	}
	res, err := s.ForkSnapshot(context.Background(), ForkSnapshotRequest{
		ForkID:      "fork-1",
		SnapshotDir: dir,
		PauseSource: true,
	})
	if err != nil || !res.OK {
		t.Fatalf("ForkSnapshot: err=%v res=%+v", err, res)
	}
	if gotSrc != clonePath {
		t.Fatalf("froze wrong rootfs: got src %q want %q", gotSrc, clonePath)
	}
	if want := filepath.Join(dir, "rootfs.ext4"); gotDst != want {
		t.Fatalf("froze rootfs to wrong path: got %q want %q", gotDst, want)
	}
	// The freeze MUST land inside the paused window: pause -> snapshot ->
	// reflink(freeze) -> resume.
	firstIdx := map[string]int{}
	for i, c := range f.callOrder {
		if _, seen := firstIdx[c]; !seen {
			firstIdx[c] = i
		}
	}
	if !(firstIdx["pause"] < firstIdx["snapshot"] &&
		firstIdx["snapshot"] < firstIdx["reflink"] &&
		firstIdx["reflink"] < firstIdx["resume"]) {
		t.Fatalf("freeze must happen inside the paused window (pause<snapshot<reflink<resume); order=%v", f.callOrder)
	}
}

// TestForkSnapshotNoRootfsCloneSkipsFreeze keeps the mock/CI paths (no on-disk
// rootfs) unchanged: with no per-activation clone there is nothing to freeze, so
// the reflink seam is never called, and the source still resumes.
func TestForkSnapshotNoRootfsCloneSkipsFreeze(t *testing.T) {
	f := &fakeVMM{}
	reflinkCalled := false
	s := &Stub{state: StateActive, vm: f}
	s.reflink = func(src, dst string) error {
		reflinkCalled = true
		return nil
	}
	res, err := s.ForkSnapshot(context.Background(), ForkSnapshotRequest{
		ForkID:      "fork-1",
		SnapshotDir: t.TempDir(),
		PauseSource: true,
	})
	if err != nil || !res.OK {
		t.Fatalf("ForkSnapshot: err=%v res=%+v", err, res)
	}
	if reflinkCalled {
		t.Fatalf("no per-activation rootfs clone: freeze must be skipped")
	}
	if !f.resumed {
		t.Fatalf("source must resume even when there is no rootfs to freeze")
	}
}

// TestForkSnapshotResumesSourceOnReflinkFailure covers the reflink-failure branch
// of the freeze: when freezing the source rootfs fails, the op must STILL resume
// the source (never leave a tenant's live sandbox frozen) before failing closed.
func TestForkSnapshotResumesSourceOnReflinkFailure(t *testing.T) {
	f := &fakeVMM{}
	cloneDir := t.TempDir()
	clonePath := filepath.Join(cloneDir, "rootfs.ext4")
	if err := os.WriteFile(clonePath, []byte("live-source-disk"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := &Stub{state: StateActive, vm: f, rootfsClonePath: clonePath}
	s.reflink = func(src, dst string) error {
		return errors.New("reflink boom")
	}
	res, err := s.ForkSnapshot(context.Background(), ForkSnapshotRequest{
		ForkID:      "fork-1",
		SnapshotDir: t.TempDir(),
		PauseSource: true,
	})
	if err == nil || res.OK {
		t.Fatalf("reflink failure must fail closed: err=%v res=%+v", err, res)
	}
	if !f.resumed {
		t.Fatalf("source MUST be resumed after a reflink failure; leaving it paused frozen breaks a post-fork exec against the source")
	}
}

// TestForkSnapshotResumeRetrySucceedsOnTransientError proves a TRANSIENT resume
// error does not recreate the v1.24.1 stuck-paused incident: the first Resume
// fails, the bounded retry tries again, the source comes back, and the op returns
// OK=true with the source resumed.
func TestForkSnapshotResumeRetrySucceedsOnTransientError(t *testing.T) {
	f := &fakeVMM{resumeErrSeq: []error{errors.New("resume blip")}}
	s := &Stub{state: StateActive, vm: f}
	s.sleep = func(time.Duration) {} // no real sleeps in the retry
	res, err := s.ForkSnapshot(context.Background(), ForkSnapshotRequest{
		ForkID:      "fork-1",
		SnapshotDir: t.TempDir(),
		PauseSource: true,
	})
	if err != nil || !res.OK {
		t.Fatalf("a transient resume error that clears on retry must yield OK: err=%v res=%+v", err, res)
	}
	if !f.resumed {
		t.Fatalf("source must be resumed after the retry succeeds")
	}
	if f.resumeCalls != 2 {
		t.Fatalf("expected exactly 2 resume attempts (one blip then success), got %d", f.resumeCalls)
	}
}

// TestForkSnapshotPersistentResumeFailureFiresMarker proves a PERSISTENT resume
// failure fails closed, leaves the source NOT resumed, exhausts the bounded retry,
// and fires the observability marker so a source left paused is visible.
func TestForkSnapshotPersistentResumeFailureFiresMarker(t *testing.T) {
	f := &fakeVMM{resumeErr: errors.New("resume rejected")}
	markerFired := 0
	s := &Stub{state: StateActive, vm: f}
	s.sleep = func(time.Duration) {}
	s.onSourceLeftPaused = func() { markerFired++ }
	res, err := s.ForkSnapshot(context.Background(), ForkSnapshotRequest{
		ForkID:      "fork-1",
		SnapshotDir: t.TempDir(),
		PauseSource: true,
	})
	if err == nil || res.OK {
		t.Fatalf("a persistent resume failure must fail closed: err=%v res=%+v", err, res)
	}
	if f.resumed {
		t.Fatalf("source must NOT be marked resumed when every resume attempt failed")
	}
	if f.resumeCalls != resumeMaxAttempts {
		t.Fatalf("expected %d resume attempts before giving up, got %d", resumeMaxAttempts, f.resumeCalls)
	}
	if markerFired != 1 {
		t.Fatalf("the source-left-paused marker must fire exactly once, got %d", markerFired)
	}
}

func TestForkSnapshotRequiresActiveState(t *testing.T) {
	f := &fakeVMM{}
	s := &Stub{state: StateDormant, vm: f}
	res, err := s.ForkSnapshot(context.Background(), ForkSnapshotRequest{ForkID: "f", SnapshotDir: t.TempDir()})
	if err == nil || res.OK {
		t.Fatalf("ForkSnapshot must refuse a non-active stub: err=%v res=%+v", err, res)
	}
}

func TestForkSnapshotFailClosedOnSnapshotError(t *testing.T) {
	f := &fakeVMM{snapErr: errSnap}
	s := activeStubWithFake(f)
	res, err := s.ForkSnapshot(context.Background(), ForkSnapshotRequest{ForkID: "f", SnapshotDir: t.TempDir()})
	if err == nil || res.OK {
		t.Fatalf("snapshot error must fail closed: err=%v res=%+v", err, res)
	}
}

func TestForkSnapshotConfinedToForksDir(t *testing.T) {
	forks := t.TempDir()
	f := &fakeVMM{}
	s := &Stub{state: StateActive, vm: f, forksDir: forks}

	// A dir WITHIN the configured forks dir is accepted.
	inside := filepath.Join(forks, "fork-1")
	res, err := s.ForkSnapshot(context.Background(), ForkSnapshotRequest{ForkID: "fork-1", SnapshotDir: inside})
	if err != nil || !res.OK {
		t.Fatalf("fork snapshot inside forks dir must succeed: err=%v res=%+v", err, res)
	}

	// A dir OUTSIDE the forks dir (here a traversal escape) is refused fail-closed
	// and the VM is never paused.
	f2 := &fakeVMM{}
	s2 := &Stub{state: StateActive, vm: f2, forksDir: forks}
	escape := filepath.Join(forks, "..", "escape")
	res2, err2 := s2.ForkSnapshot(context.Background(), ForkSnapshotRequest{ForkID: "x", SnapshotDir: escape})
	if err2 == nil || res2.OK {
		t.Fatalf("fork snapshot outside forks dir must fail closed: err=%v res=%+v", err2, res2)
	}
	if f2.paused {
		t.Fatalf("an out-of-bounds fork snapshot must not pause the VM")
	}
}

func TestRemoveForkSnapshotConfinedToForksDir(t *testing.T) {
	forks := t.TempDir()
	s := &Stub{state: StateActive, vm: &fakeVMM{}, forksDir: forks}
	if err := s.RemoveForkSnapshot(ForkSnapshotRequest{SnapshotDir: filepath.Join(forks, "..", "escape")}); err == nil {
		t.Fatalf("remove fork snapshot outside forks dir must be refused")
	}
}
