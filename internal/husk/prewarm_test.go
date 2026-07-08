package husk

import (
	"context"
	"sync"
	"testing"
	"time"

	"mitos.run/mitos/internal/firecracker"
)

// vmRegistry is a concurrency-safe map of derived-config-ID -> started fake VMM.
// The pre-warm re-warms the slot on a BACKGROUND goroutine (off the fork hot
// path), so its start callback writes this map concurrently with a test's reads;
// a plain map would data-race even across distinct keys. Every access takes mu.
type vmRegistry struct {
	mu sync.Mutex
	m  map[string]*fakeVMM
}

func newVMRegistry() *vmRegistry { return &vmRegistry{m: map[string]*fakeVMM{}} }

func (r *vmRegistry) put(id string, vm *fakeVMM) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.m[id] = vm
}

func (r *vmRegistry) get(id string) *fakeVMM {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.m[id]
}

// newPrewarmTestStub builds a multi-VM stub with the pre-warm slot ON, using a
// keyed starter so a test can look up the per-VM fake by its derived VMConfig ID
// and prove which Firecracker a fork ran on.
func newPrewarmTestStub(t *testing.T, vms *vmRegistry) *Stub {
	t.Helper()
	start := func(cfg firecracker.VMConfig) (vmm, error) {
		vm := &fakeVMM{}
		vms.put(cfg.ID, vm)
		return vm, nil
	}
	return New(firecracker.VMConfig{ID: "husk-test"}, Options{
		Start:        start,
		Ready:        readyOK,
		Notify:       (&fakeNotifier{}).notify,
		Verify:       verifyOK,
		MultiVM:      true,
		PrewarmChild: true,
	})
}

const prewarmSlotCfgID = "husk-test-" + string(prewarmSlotVMID)

// instVM reads one instance's VMM handle under the same locks the engine uses, so
// a test assertion never races the background re-warm goroutine's map/instance
// writes. Returns nil when the instance does not exist.
func instVM(s *Stub, id vmID) vmm {
	s.mu.Lock()
	inst := s.instances[id]
	s.mu.Unlock()
	if inst == nil {
		return nil
	}
	inst.mu.Lock()
	defer inst.mu.Unlock()
	return inst.vm
}

// instState reads one instance's lifecycle State under locks. Returns StateNew for
// a missing instance.
func instState(s *Stub, id vmID) State {
	s.mu.Lock()
	inst := s.instances[id]
	s.mu.Unlock()
	if inst == nil {
		return StateNew
	}
	inst.mu.Lock()
	defer inst.mu.Unlock()
	return inst.state
}

// waitForPrewarmSlot polls until the reserved slot holds a DORMANT VMM (the
// re-warm goroutine runs off the fork hot path), returning that VMM handle. It
// fails the test if the slot is not re-warmed within the deadline.
func waitForPrewarmSlot(t *testing.T, s *Stub) vmm {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if instState(s, prewarmSlotVMID) == StateDormant {
			if vm := instVM(s, prewarmSlotVMID); vm != nil {
				return vm
			}
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("pre-warm slot was not re-warmed to StateDormant in time")
	return nil
}

// TestPrewarmedChildIsConsumedAndReWarmed proves the core of the pre-warm: an
// eagerly pre-warmed dormant child is ADOPTED by a fork (so the fork skips the
// on-demand process boot, fc_boot ~0, and reaches StateActive on the SAME
// Firecracker that was pre-booted), and a FRESH dormant child is re-warmed for the
// next fork, off the fork hot path.
func TestPrewarmedChildIsConsumedAndReWarmed(t *testing.T) {
	vms := newVMRegistry()
	s := newPrewarmTestStub(t, vms)

	// Bring up the source (default) VM, then eagerly warm the child slot.
	if err := s.prepareInstance(context.Background(), defaultVMID, "", nil); err != nil {
		t.Fatalf("prepareInstance(default): %v", err)
	}
	if _, err := s.activateInstance(context.Background(), defaultVMID, ActivateRequest{SnapshotDir: "/snap"}); err != nil {
		t.Fatalf("activateInstance(default): %v", err)
	}
	if err := s.PrewarmChild(context.Background()); err != nil {
		t.Fatalf("PrewarmChild: %v", err)
	}
	warmed := vms.get(prewarmSlotCfgID)
	if warmed == nil {
		t.Fatalf("pre-warm must boot a dormant child under %q", prewarmSlotCfgID)
	}
	if got := instState(s, prewarmSlotVMID); got != StateDormant {
		t.Fatalf("pre-warmed slot state = %s, want dormant", got)
	}

	// A fork ADOPTS the pre-warmed child: no on-demand boot for the fork's own vmID.
	res := s.SpawnVM(context.Background(), SpawnVMRequest{
		VMID:     "fork-1",
		Activate: ActivateRequest{SnapshotDir: "/snap"},
	})
	if !res.OK {
		t.Fatalf("SpawnVM adopting the pre-warmed child must succeed: %+v", res)
	}
	// The fork ran on the SAME Firecracker that was pre-booted (adoption), and no
	// fresh process was started under the fork's own derived id.
	if instVM(s, "fork-1") != warmed {
		t.Fatal("fork must ADOPT the pre-warmed child's Firecracker, not boot its own")
	}
	if booted := vms.get("husk-test-fork-1"); booted != nil {
		t.Fatal("adopting a pre-warmed child must NOT boot a Firecracker under the fork's derived id")
	}
	if got := instState(s, "fork-1"); got != StateActive {
		t.Fatalf("adopted fork state = %s, want active", got)
	}
	// The on-fork-path boot is pre-paid: fc_boot is recorded as 0, and no
	// verify_prepare ran on the hot path (both happened during the pre-warm).
	if fc, ok := res.Stages["fc_boot"]; !ok || fc != 0 {
		t.Fatalf("adopted fork fc_boot stage = %v (ok=%v), want 0 (boot pre-paid off the hot path)", fc, ok)
	}
	if _, ran := res.Stages["verify_prepare"]; ran {
		t.Fatalf("adopted fork must not re-run the snapshot verify on the hot path, got stages %v", res.Stages)
	}

	// A fresh dormant child is re-warmed for the NEXT fork, and it is a DISTINCT
	// Firecracker from the one just consumed.
	next := waitForPrewarmSlot(t, s)
	if next == warmed {
		t.Fatal("re-warm must boot a FRESH dormant child, not re-use the consumed one")
	}
	if rewarmed := vms.get(prewarmSlotCfgID); rewarmed == warmed {
		t.Fatal("re-warm must start a new Firecracker under the reserved slot id")
	}
}

// TestPrewarmMissBootsOnDemandThenWarms proves the fallback: with the slot not yet
// warm, the FIRST fork boots on demand (fc_boot recorded, byte-for-byte the prior
// behavior) and the slot is warmed for a LATER fork, off the hot path.
func TestPrewarmMissBootsOnDemandThenWarms(t *testing.T) {
	vms := newVMRegistry()
	s := newPrewarmTestStub(t, vms)
	if err := s.prepareInstance(context.Background(), defaultVMID, "", nil); err != nil {
		t.Fatalf("prepareInstance(default): %v", err)
	}

	// No eager warm: the first fork misses and boots its OWN Firecracker on demand.
	res := s.SpawnVM(context.Background(), SpawnVMRequest{
		VMID:     "fork-1",
		Activate: ActivateRequest{SnapshotDir: "/snap"},
	})
	if !res.OK {
		t.Fatalf("SpawnVM on a pre-warm miss must still succeed on demand: %+v", res)
	}
	if booted := vms.get("husk-test-fork-1"); booted == nil {
		t.Fatal("a pre-warm miss must boot the fork's own Firecracker on demand")
	}
	if _, ok := res.Stages["fc_boot"]; !ok {
		t.Fatalf("an on-demand fork must record a real fc_boot stage, got %v", res.Stages)
	}

	// The miss kicked a re-warm so a LATER fork can skip the boot.
	warmed := waitForPrewarmSlot(t, s)
	res2 := s.SpawnVM(context.Background(), SpawnVMRequest{
		VMID:     "fork-2",
		Activate: ActivateRequest{SnapshotDir: "/snap"},
	})
	if !res2.OK {
		t.Fatalf("second SpawnVM must adopt the warmed child: %+v", res2)
	}
	if instVM(s, "fork-2") != warmed {
		t.Fatal("the second fork must ADOPT the warmed child booted after the first fork")
	}
	if fc := res2.Stages["fc_boot"]; fc != 0 {
		t.Fatalf("the second (adopting) fork fc_boot = %v, want 0", fc)
	}
}

// TestPrewarmOffKeepsOnDemandBoot proves pre-warm OFF (the default) is
// byte-for-byte the on-demand path: no reserved slot is ever created and every
// fork boots its own Firecracker. It uses the plain-map stub (no re-warm
// goroutine runs when pre-warm is off, so no concurrency-safe registry is needed).
func TestPrewarmOffKeepsOnDemandBoot(t *testing.T) {
	vms := map[string]*fakeVMM{}
	s := newMultiVMTestStub(t, vms) // PrewarmChild defaults off
	if err := s.prepareInstance(context.Background(), defaultVMID, "", nil); err != nil {
		t.Fatalf("prepareInstance(default): %v", err)
	}
	if vm := s.consumePrewarmedChild(); vm != nil {
		t.Fatal("consumePrewarmedChild must return nil when pre-warm is off")
	}
	res := s.SpawnVM(context.Background(), SpawnVMRequest{
		VMID:     "fork-1",
		Activate: ActivateRequest{SnapshotDir: "/snap"},
	})
	if !res.OK {
		t.Fatalf("SpawnVM: %+v", res)
	}
	if _, booted := vms["husk-test-fork-1"]; !booted {
		t.Fatal("with pre-warm off a fork must boot its own Firecracker")
	}
	if _, warmed := s.instances[prewarmSlotVMID]; warmed {
		t.Fatal("with pre-warm off no reserved slot may be created")
	}
}
