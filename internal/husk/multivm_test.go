package husk

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"mitos.run/mitos/internal/firecracker"
	"mitos.run/mitos/internal/fork"

	"mitos.run/mitos/internal/guestgrpc"
)

// newMultiVMTestStub builds a stub with the multi-VM execution mode ON, using a
// keyed starter so a test can look up the per-VM fake by the derived VMConfig ID
// (deriveVMConfig gives each vmID a distinct ID). The other seams are the same
// no-op fakes the single-VM unit path uses, so these tests exercise the per-VM
// state machine multiplexing with the mock, no real Firecracker or KVM.
func newMultiVMTestStub(t *testing.T, vms map[string]*fakeVMM) *Stub {
	t.Helper()
	start := func(cfg firecracker.VMConfig) (vmm, error) {
		vm := &fakeVMM{}
		vms[cfg.ID] = vm
		return vm, nil
	}
	return New(firecracker.VMConfig{ID: "husk-test"}, Options{
		Start:   start,
		Ready:   readyOK,
		Notify:  (&fakeNotifier{}).notify,
		Verify:  verifyOK,
		MultiVM: true,
	})
}

// TestMultiVMTwoInstancesReachActiveIndependently proves the core of increment 2
// of #764: with the flag ON the stub can Prepare+Activate TWO distinct vmIDs and
// both reach StateActive independently, each keyed in the instances map with its
// OWN VMM handle and generation counter. It drives the per-VM engine directly
// (prepareInstance/activateInstance), which the public dispatch routes to.
func TestMultiVMTwoInstancesReachActiveIndependently(t *testing.T) {
	vms := map[string]*fakeVMM{}
	s := newMultiVMTestStub(t, vms)

	const second vmID = "vm-2"
	for _, id := range []vmID{defaultVMID, second} {
		if err := s.prepareInstance(context.Background(), id, "", nil); err != nil {
			t.Fatalf("prepareInstance(%s): %v", id, err)
		}
		res, err := s.activateInstance(context.Background(), id, ActivateRequest{SnapshotDir: "/snap"})
		if err != nil || !res.OK {
			t.Fatalf("activateInstance(%s): err=%v ok=%v", id, err, res.OK)
		}
	}

	if got := s.instances[defaultVMID].state; got != StateActive {
		t.Fatalf("default instance state = %s, want active", got)
	}
	if got := s.instances[second].state; got != StateActive {
		t.Fatalf("second instance state = %s, want active", got)
	}
	// Distinct VMM handles: each vmID owns its own Firecracker process (its own
	// socket/workdir), never a shared one.
	if s.instances[defaultVMID].vm == s.instances[second].vm {
		t.Fatal("two vmIDs must own distinct VMM handles")
	}
	// Each per-VM generation counter starts at 1 (one reseed per activation).
	if g := s.instances[defaultVMID].generation; g != 1 {
		t.Fatalf("default generation = %d, want 1", g)
	}
	if g := s.instances[second].generation; g != 1 {
		t.Fatalf("second generation = %d, want 1", g)
	}
	// The two fakes are the two distinct derived-ID VMMs the keyed starter built.
	if len(vms) != 2 {
		t.Fatalf("expected 2 distinct started VMMs, got %d: %v", len(vms), vms)
	}
	if _, ok := vms["husk-test"]; !ok {
		t.Fatalf("default vmID must derive the base config ID, got keys %v", vms)
	}
	if _, ok := vms["husk-test-vm-2"]; !ok {
		t.Fatalf("second vmID must derive a distinct per-VM config ID, got keys %v", vms)
	}
}

// TestMultiVMCloseOneLeavesOtherActive proves per-VM isolation: closing ONE
// instance tears down only that VMM and returns it to StateNew, while the other
// instance stays StateActive with its VMM untouched. This is the isolated
// state-machine property the density work needs (a sibling dying must not take
// its siblings down).
func TestMultiVMCloseOneLeavesOtherActive(t *testing.T) {
	vms := map[string]*fakeVMM{}
	s := newMultiVMTestStub(t, vms)

	const second vmID = "vm-2"
	for _, id := range []vmID{defaultVMID, second} {
		if err := s.prepareInstance(context.Background(), id, "", nil); err != nil {
			t.Fatalf("prepareInstance(%s): %v", id, err)
		}
		if _, err := s.activateInstance(context.Background(), id, ActivateRequest{SnapshotDir: "/snap"}); err != nil {
			t.Fatalf("activateInstance(%s): %v", id, err)
		}
	}

	if err := s.closeInstance(defaultVMID); err != nil {
		t.Fatalf("closeInstance(default): %v", err)
	}

	// The closed instance is torn down (VMM closed, back to StateNew).
	if !vms["husk-test"].closed {
		t.Fatal("closing the default instance must tear down its VMM")
	}
	if got := s.instances[defaultVMID].state; got != StateNew {
		t.Fatalf("closed instance state = %s, want new", got)
	}
	// The other instance is untouched: still active, its VMM not closed.
	if vms["husk-test-vm-2"].closed {
		t.Fatal("closing one instance must NOT close a sibling's VMM")
	}
	if got := s.instances[second].state; got != StateActive {
		t.Fatalf("sibling instance state = %s, want active (unaffected by the other's close)", got)
	}
}

// TestMultiVMMeteringReportsBothVMs proves the metering path reports EVERY active
// VM in the pod, one sample per vmID keyed on its derived id, so a multi-VM pod
// meters all the same-tenant forks it hosts (not just one).
func TestMultiVMMeteringReportsBothVMs(t *testing.T) {
	vms := map[string]*fakeVMM{}
	s := newMultiVMTestStub(t, vms)

	for _, id := range []vmID{defaultVMID, "vm-2"} {
		if err := s.prepareInstance(context.Background(), id, "", nil); err != nil {
			t.Fatalf("prepareInstance(%s): %v", id, err)
		}
		if _, err := s.activateInstance(context.Background(), id, ActivateRequest{SnapshotDir: "/snap"}); err != nil {
			t.Fatalf("activateInstance(%s): %v", id, err)
		}
	}

	rep := s.Metering()
	if len(rep.Sandboxes) != 2 {
		t.Fatalf("multi-VM metering must report both VMs, got %d samples", len(rep.Sandboxes))
	}
	ids := map[string]bool{}
	for _, sb := range rep.Sandboxes {
		ids[sb.ID] = true
	}
	if !ids["husk-test"] || !ids["husk-test-vm-2"] {
		t.Fatalf("metering must carry both derived vm-ids, got %v", ids)
	}
}

// TestMultiVMSecondActivateFailClosedLeavesFirstActive proves a fail-closed
// activate of a SECOND VM (its snapshot load fails) never disturbs an
// already-active sibling: the first stays StateActive, the second stays
// StateDormant and reports not-OK.
func TestMultiVMSecondActivateFailClosedLeavesFirstActive(t *testing.T) {
	// A starter that fails the SECOND VM's snapshot load only.
	vms := map[string]*fakeVMM{}
	start := func(cfg firecracker.VMConfig) (vmm, error) {
		vm := &fakeVMM{}
		if cfg.ID == "husk-test-vm-2" {
			vm.loadErr = errors.New("snapshot corrupt")
		}
		vms[cfg.ID] = vm
		return vm, nil
	}
	s := New(firecracker.VMConfig{ID: "husk-test"}, Options{
		Start:   start,
		Ready:   readyOK,
		Notify:  (&fakeNotifier{}).notify,
		Verify:  verifyOK,
		MultiVM: true,
	})

	if err := s.prepareInstance(context.Background(), defaultVMID, "", nil); err != nil {
		t.Fatalf("prepareInstance(default): %v", err)
	}
	if _, err := s.activateInstance(context.Background(), defaultVMID, ActivateRequest{SnapshotDir: "/snap"}); err != nil {
		t.Fatalf("activateInstance(default): %v", err)
	}
	if err := s.prepareInstance(context.Background(), "vm-2", "", nil); err != nil {
		t.Fatalf("prepareInstance(vm-2): %v", err)
	}
	res, err := s.activateInstance(context.Background(), "vm-2", ActivateRequest{SnapshotDir: "/snap"})
	if err == nil || res.OK {
		t.Fatal("second activate must fail closed on a bad snapshot load")
	}

	if got := s.instances["vm-2"].state; got == StateActive {
		t.Fatalf("failed-closed second instance must not be active, got %s", got)
	}
	if got := s.instances[defaultVMID].state; got != StateActive {
		t.Fatalf("first instance must stay active despite the sibling's failure, got %s", got)
	}
}

// TestMultiVMPublicDispatchRoutesToDefault proves the public lifecycle methods
// (Prepare/Activate/Close/Metering) dispatch to the per-VM engine under the flag:
// a plain Prepare+Activate with no VMID selector drives the default instance, and
// Close tears every instance down. This is the compatibility seam: an existing
// single-entry caller keeps working, now backed by the instances map.
func TestMultiVMPublicDispatchRoutesToDefault(t *testing.T) {
	vms := map[string]*fakeVMM{}
	s := newMultiVMTestStub(t, vms)

	if err := s.Prepare(context.Background()); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if got := s.instances[defaultVMID].state; got != StateDormant {
		t.Fatalf("after Prepare default instance = %s, want dormant", got)
	}
	res, err := s.Activate(context.Background(), ActivateRequest{SnapshotDir: "/snap"})
	if err != nil || !res.OK {
		t.Fatalf("Activate: err=%v ok=%v", err, res.OK)
	}
	if got := s.instances[defaultVMID].state; got != StateActive {
		t.Fatalf("after Activate default instance = %s, want active", got)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := s.instances[defaultVMID].state; got != StateNew {
		t.Fatalf("after Close default instance = %s, want new", got)
	}
	if !vms["husk-test"].closed {
		t.Fatal("Close must tear the default VMM down")
	}
}

// TestMultiVMActivateRoutesByVMID proves the explicit vmID selector on the
// control request: with the flag on, Activate routes to the instance named by
// req.VMID (defaulting to defaultVMID when empty), so the control API can address
// a specific same-tenant VM in the pod.
func TestMultiVMActivateRoutesByVMID(t *testing.T) {
	vms := map[string]*fakeVMM{}
	s := newMultiVMTestStub(t, vms)

	const second vmID = "vm-2"
	if err := s.prepareInstance(context.Background(), second, "", nil); err != nil {
		t.Fatalf("prepareInstance(vm-2): %v", err)
	}
	res, err := s.Activate(context.Background(), ActivateRequest{SnapshotDir: "/snap", VMID: string(second)})
	if err != nil || !res.OK {
		t.Fatalf("Activate(vm-2): err=%v ok=%v", err, res.OK)
	}
	if got := s.instances[second].state; got != StateActive {
		t.Fatalf("VMID-selected instance = %s, want active", got)
	}
	// The default instance was never prepared, so it must be untouched.
	if inst := s.instances[defaultVMID]; inst.state != StateNew {
		t.Fatalf("unaddressed default instance = %s, want new", inst.state)
	}
}

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

// TestMultiVMPrepareRunsConcurrentlyPerInstance proves the per-instance-lock fix
// from the #772 review: two distinct vmIDs Prepare CONCURRENTLY, neither waiting
// on the other's blocking VMM start. The injected starter signals when it has
// begun and then blocks on a channel, so if the stub held one shared lock across
// the blocking start (the pre-fix behavior) the second Prepare could never even
// enter start until the first returned, and the barrier below would time out.
// With a lock per vmInstance both starts reach the blocked state at once; we then
// release them and assert both instances reach StateDormant.
func TestMultiVMPrepareRunsConcurrentlyPerInstance(t *testing.T) {
	const barrier = 2
	entered := make(chan struct{}, barrier)
	release := make(chan struct{})

	var mu sync.Mutex
	vms := map[string]*fakeVMM{}
	start := func(cfg firecracker.VMConfig) (vmm, error) {
		// Announce this start has begun, then block until released. Two starts
		// blocked here simultaneously is the concurrency the fix delivers.
		entered <- struct{}{}
		<-release
		vm := &fakeVMM{}
		mu.Lock()
		vms[cfg.ID] = vm
		mu.Unlock()
		return vm, nil
	}
	s := New(firecracker.VMConfig{ID: "husk-test"}, Options{
		Start:   start,
		Ready:   readyOK,
		Notify:  (&fakeNotifier{}).notify,
		Verify:  verifyOK,
		MultiVM: true,
	})

	ids := []vmID{defaultVMID, "vm-2"}
	errs := make([]error, len(ids))
	var wg sync.WaitGroup
	for i, id := range ids {
		wg.Add(1)
		go func(i int, id vmID) {
			defer wg.Done()
			errs[i] = s.prepareInstance(context.Background(), id, "", nil)
		}(i, id)
	}

	// Both starts must reach the blocked state before either is released. A
	// shared lock held across the blocking start would let only one goroutine in,
	// so the second receive would time out.
	for i := 0; i < barrier; i++ {
		select {
		case <-entered:
		case <-time.After(2 * time.Second):
			t.Fatalf("only %d of %d prepares reached the blocking start; per-instance lifecycles are serialized", i, barrier)
		}
	}

	// Both are blocked concurrently: release and let both finish.
	close(release)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("prepareInstance(%s): %v", ids[i], err)
		}
	}
	if got := s.instances[defaultVMID].state; got != StateDormant {
		t.Fatalf("default instance state = %s, want dormant", got)
	}
	if got := s.instances["vm-2"].state; got != StateDormant {
		t.Fatalf("vm-2 instance state = %s, want dormant", got)
	}
}

// TestMultiVMActivateRunsConcurrentlyPerInstance proves the same for the activate
// hot path: two already-dormant vmIDs Activate CONCURRENTLY, each blocking in its
// own guest-ready wait, without one serializing behind the other. The injected
// readiness seam signals entry then blocks; a shared lock held across the ready
// wait (the pre-fix behavior) would let only one activate reach it.
func TestMultiVMActivateRunsConcurrentlyPerInstance(t *testing.T) {
	const barrier = 2
	entered := make(chan struct{}, barrier)
	release := make(chan struct{})

	vms := map[string]*fakeVMM{}
	start := func(cfg firecracker.VMConfig) (vmm, error) {
		vm := &fakeVMM{}
		vms[cfg.ID] = vm
		return vm, nil
	}
	ready := func(context.Context, string, time.Duration) (*guestgrpc.Client, error) {
		entered <- struct{}{}
		<-release
		return nil, nil
	}
	s := New(firecracker.VMConfig{ID: "husk-test"}, Options{
		Start:   start,
		Ready:   ready,
		Notify:  (&fakeNotifier{}).notify,
		Verify:  verifyOK,
		MultiVM: true,
	})

	ids := []vmID{defaultVMID, "vm-2"}
	for _, id := range ids {
		if err := s.prepareInstance(context.Background(), id, "", nil); err != nil {
			t.Fatalf("prepareInstance(%s): %v", id, err)
		}
	}

	results := make([]ActivateResult, len(ids))
	errs := make([]error, len(ids))
	var wg sync.WaitGroup
	for i, id := range ids {
		wg.Add(1)
		go func(i int, id vmID) {
			defer wg.Done()
			results[i], errs[i] = s.activateInstance(context.Background(), id, ActivateRequest{SnapshotDir: "/snap"})
		}(i, id)
	}

	for i := 0; i < barrier; i++ {
		select {
		case <-entered:
		case <-time.After(2 * time.Second):
			t.Fatalf("only %d of %d activates reached the blocking guest-ready wait; per-instance lifecycles are serialized", i, barrier)
		}
	}
	close(release)
	wg.Wait()

	for i, id := range ids {
		if errs[i] != nil || !results[i].OK {
			t.Fatalf("activateInstance(%s): err=%v ok=%v", id, errs[i], results[i].OK)
		}
		if got := s.instances[id].state; got != StateActive {
			t.Fatalf("instance %s state = %s, want active", id, got)
		}
	}
}

// TestMultiVMRejectsUnsafeVMID proves a vmID that could traverse out of the pod
// workdir is refused before any path is derived from it (issue #772 review).
func TestMultiVMRejectsUnsafeVMID(t *testing.T) {
	for _, bad := range []vmID{"../evil", "a/b", "", "with space", "..", ".", "x/../../y"} {
		if err := checkVMID(bad); err == nil {
			t.Errorf("checkVMID(%q) = nil, want an error (unsafe vm id must be rejected)", string(bad))
		}
	}
	for _, ok := range []vmID{defaultVMID, "vm1", "fork-0", "a_b-9", "A0"} {
		if err := checkVMID(ok); err != nil {
			t.Errorf("checkVMID(%q) = %v, want nil (valid vm id)", string(ok), err)
		}
	}
}

// TestMultiVMPrepareRefusedWhileClosing proves a prepareInstance create cannot
// add a VM after closeAllInstances has begun teardown (issue #795 review): once
// s.closing is set, instanceFor refuses the create and prepareInstance errors,
// so no VM outlives Close.
func TestMultiVMPrepareRefusedWhileClosing(t *testing.T) {
	vms := map[string]*fakeVMM{}
	s := newMultiVMTestStub(t, vms)
	s.mu.Lock()
	s.closing = true
	s.mu.Unlock()
	if got := s.instanceFor("late", true); got != nil {
		t.Fatalf("instanceFor create during closing = %v, want nil (refused)", got)
	}
	if err := s.prepareInstance(context.Background(), "late", "", nil); err == nil {
		t.Fatal("prepareInstance during closing = nil error, want a closing error")
	}
	// Also refuse re-preparing an id that ALREADY has a map entry (Close leaves
	// entries at StateNew), not just a brand-new id.
	s.mu.Lock()
	s.closing = false
	s.instances["existing"] = newVMInstance()
	s.closing = true
	s.mu.Unlock()
	if got := s.instanceFor("existing", true); got != nil {
		t.Fatalf("instanceFor create for an existing entry during closing = %v, want nil (refused)", got)
	}
	if err := s.prepareInstance(context.Background(), "existing", "", nil); err == nil {
		t.Fatal("prepareInstance of an existing entry during closing = nil error, want a closing error")
	}
	s.mu.Lock()
	_, exists := s.instances["late"]
	s.mu.Unlock()
	if exists {
		t.Fatal("a vm was added to the instances map during closing; it would outlive Close")
	}
}

// TestSpawnVMActivationFailureReturnsNotOK proves the full SpawnVM path reports
// a failure fail-closed when activation errors: the spawned VM's snapshot load
// fails, so SpawnVM returns OK=false carrying the activation error, the instance
// is not left Active, and an already-active sibling is undisturbed.
func TestSpawnVMActivationFailureReturnsNotOK(t *testing.T) {
	vms := map[string]*fakeVMM{}
	start := func(cfg firecracker.VMConfig) (vmm, error) {
		vm := &fakeVMM{}
		if cfg.ID == "husk-test-fork-9" {
			vm.loadErr = errors.New("snapshot corrupt")
		}
		vms[cfg.ID] = vm
		return vm, nil
	}
	s := New(firecracker.VMConfig{ID: "husk-test"}, Options{
		Start:   start,
		Ready:   readyOK,
		Notify:  (&fakeNotifier{}).notify,
		Verify:  verifyOK,
		MultiVM: true,
	})
	// Bring up the primary VM so we can prove it is undisturbed by the failure.
	if err := s.prepareInstance(context.Background(), defaultVMID, "", nil); err != nil {
		t.Fatalf("prepareInstance(default): %v", err)
	}
	if _, err := s.activateInstance(context.Background(), defaultVMID, ActivateRequest{SnapshotDir: "/snap"}); err != nil {
		t.Fatalf("activateInstance(default): %v", err)
	}

	res := s.SpawnVM(context.Background(), SpawnVMRequest{
		VMID:     "fork-9",
		Activate: ActivateRequest{SnapshotDir: "/snap"},
	})
	if res.OK {
		t.Fatal("SpawnVM must report not-OK when activation fails")
	}
	if res.Error == "" {
		t.Fatal("a failed SpawnVM must carry the activation error")
	}
	if res.VMID != "fork-9" {
		t.Fatalf("SpawnVM result VMID = %q, want fork-9", res.VMID)
	}
	if got := s.instances["fork-9"].state; got == StateActive {
		t.Fatalf("a fail-closed spawned instance must not be active, got %s", got)
	}
	if got := s.instances[defaultVMID].state; got != StateActive {
		t.Fatalf("the primary instance must stay active despite the spawn failure, got %s", got)
	}
}

// failingMemSourceHandle is a WPForkHandle whose SetMemSource always fails. Only the
// one method under test does anything; the rest satisfy the interface.
type failingMemSourceHandle struct{ err error }

func (h *failingMemSourceHandle) Receive() error                 { return nil }
func (h *failingMemSourceHandle) Freeze() (time.Duration, error) { return 0, nil }
func (h *failingMemSourceHandle) Serve() error                   { return nil }
func (h *failingMemSourceHandle) SetMemSource(string) error      { return h.err }
func (h *failingMemSourceHandle) FrozenFd() int                  { return -1 }
func (h *failingMemSourceHandle) FrozenPage(uint64) bool         { return false }
func (h *failingMemSourceHandle) FrozenBitmap() []byte           { return nil }
func (h *failingMemSourceHandle) ChildImport(string) (fork.ChildMemfdImport, error) {
	return fork.ChildMemfdImport{}, nil
}
func (h *failingMemSourceHandle) FaultCount() int64             { return 0 }
func (h *failingMemSourceHandle) FreezeDuration() time.Duration { return 0 }
func (h *failingMemSourceHandle) Close() error                  { return nil }

// TestActivateInstanceFailsClosedWhenLazyMemSourceCannotArm proves the fail-closed
// gate on the LAZY live-cow restore.
//
// The source Firecracker is launched with FIRECRACKER_MITOS_LAZY_RESTORE, so it maps
// guest RAM as an EMPTY shared memfd and every page must arrive through the WP
// handler's userfaultfd MISSING faults. If the handler cannot open the snapshot mem
// file there is nothing to populate that memory: loading the snapshot anyway and
// resuming the vCPUs would run the guest on all-zero RAM. activateInstance must
// therefore abort BEFORE the load, leave the VM not active, and say why.
func TestActivateInstanceFailsClosedWhenLazyMemSourceCannotArm(t *testing.T) {
	vms := map[string]*fakeVMM{}
	s := newMultiVMTestStub(t, vms)
	s.liveCowFork = true
	s.liveCowHandle = &failingMemSourceHandle{err: errors.New("open mem source: no such file")}

	if err := s.prepareInstance(context.Background(), defaultVMID, "", nil); err != nil {
		t.Fatalf("prepareInstance: %v", err)
	}
	res, err := s.activateInstance(context.Background(), defaultVMID, ActivateRequest{SnapshotDir: "/snap"})
	if err == nil || res.OK {
		t.Fatalf("activate must fail closed when the lazy mem source cannot arm; got ok=%v err=%v", res.OK, err)
	}
	if !strings.Contains(res.Error, "mem source") {
		t.Errorf("activate error must name the failing step; got %q", res.Error)
	}
	if got := s.instances[defaultVMID].state; got == StateActive {
		t.Errorf("VM must NOT be active after a failed lazy arm; state = %s", got)
	}
	// The snapshot must never have been loaded: a loaded-but-unpopulated VM is the
	// exact hazard this gate exists to prevent.
	if vm := vms[string(defaultVMID)]; vm != nil {
		vm.mu.Lock()
		loads := vm.loadCalls
		vm.mu.Unlock()
		if loads > 0 {
			t.Errorf("snapshot was loaded %d time(s) despite the failed arm; must abort before the load", loads)
		}
	}
}
