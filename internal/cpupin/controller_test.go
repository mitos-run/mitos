package cpupin

import (
	"errors"
	"testing"
)

// recordingApplier is a test Applier that records calls and lets a test inject
// a topology and errors, so the controller orchestration is testable on darwin
// without touching the real scheduler.
type recordingApplier struct {
	topo      Topology
	topoErr   error
	pins      []PinRequest
	raised    [][]int
	dropped   [][]int
	supported bool
}

func (a *recordingApplier) ReadTopology() (Topology, error) { return a.topo, a.topoErr }
func (a *recordingApplier) ApplyPin(req PinRequest) error {
	a.pins = append(a.pins, req)
	return nil
}
func (a *recordingApplier) RaiseLaunchPriority(t []int) error {
	a.raised = append(a.raised, t)
	return nil
}
func (a *recordingApplier) DropLaunchPriority(t []int) error {
	a.dropped = append(a.dropped, t)
	return nil
}
func (a *recordingApplier) Supported() bool { return a.supported }

func ctrlTopo() Topology {
	return Topology{Cores: []PhysicalCore{
		{ID: 0, Logical: []int{0, 4}},
		{ID: 1, Logical: []int{1, 5}},
	}}
}

// TestControllerDisabledSkips proves that with pinning disabled the controller
// applies nothing: no pin, no priority raise. This is the default path.
func TestControllerDisabledSkips(t *testing.T) {
	a := &recordingApplier{topo: ctrlTopo(), supported: true}
	c := NewController(a)
	err := c.OnGuestReady(ReadyEvent{
		ForkID:    "f1",
		ThreadIDs: []int{100, 101},
		VCPUs:     1,
		Config:    Config{Enabled: false},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(a.pins) != 0 || len(a.raised) != 0 {
		t.Fatalf("disabled pinning must apply nothing: pins=%v raised=%v", a.pins, a.raised)
	}
}

// TestControllerPinsAndDrops proves the enabled path computes a plan, applies
// the pin to the fork's threads, and drops the launch priority after ready.
func TestControllerPinsAndDrops(t *testing.T) {
	a := &recordingApplier{topo: ctrlTopo(), supported: true}
	c := NewController(a)
	err := c.OnGuestReady(ReadyEvent{
		ForkID:    "f1",
		ThreadIDs: []int{100, 101},
		VCPUs:     1,
		Config:    Config{Enabled: true, Policy: PolicyPack, SiblingPairing: true, LaunchRtPriority: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(a.pins) != 1 {
		t.Fatalf("expected one pin, got %d", len(a.pins))
	}
	if got := a.pins[0].CPUs; len(got) != 2 {
		t.Fatalf("expected sibling-paired pin to 2 cpus, got %v", got)
	}
	if len(a.dropped) != 1 {
		t.Fatalf("launch priority must be dropped after ready, dropped=%v", a.dropped)
	}
}

// TestControllerTracksRunningForks proves a second ready fork packs onto a
// distinct core because the controller remembers the first fork's pin.
func TestControllerTracksRunningForks(t *testing.T) {
	a := &recordingApplier{topo: ctrlTopo(), supported: true}
	c := NewController(a)
	cfg := Config{Enabled: true, Policy: PolicyPack, SiblingPairing: true}
	if err := c.OnGuestReady(ReadyEvent{ForkID: "f1", ThreadIDs: []int{100}, VCPUs: 1, Config: cfg}); err != nil {
		t.Fatal(err)
	}
	if err := c.OnGuestReady(ReadyEvent{ForkID: "f2", ThreadIDs: []int{200}, VCPUs: 1, Config: cfg}); err != nil {
		t.Fatal(err)
	}
	c1 := a.pins[0].CPUs
	c2 := a.pins[1].CPUs
	if sharesPhysicalCore(ctrlTopo(), c1, c2) {
		t.Fatalf("controller let f1 (%v) and f2 (%v) share a physical core", c1, c2)
	}
}

// TestControllerForgetFreesCore proves Forget releases a fork's core so a later
// fork can reuse it.
func TestControllerForgetFreesCore(t *testing.T) {
	a := &recordingApplier{topo: ctrlTopo(), supported: true}
	c := NewController(a)
	cfg := Config{Enabled: true, Policy: PolicyPack, SiblingPairing: true}
	if err := c.OnGuestReady(ReadyEvent{ForkID: "f1", ThreadIDs: []int{100}, VCPUs: 1, Config: cfg}); err != nil {
		t.Fatal(err)
	}
	first := a.pins[0].CPUs
	c.Forget("f1")
	if err := c.OnGuestReady(ReadyEvent{ForkID: "f2", ThreadIDs: []int{200}, VCPUs: 1, Config: cfg}); err != nil {
		t.Fatal(err)
	}
	reused := a.pins[1].CPUs
	if reused[0] != first[0] {
		t.Fatalf("after Forget(f1) the freed core %v should be reused, got %v", first, reused)
	}
}

// TestControllerTopologyErrorPropagates proves a topology read failure surfaces
// rather than silently skipping the pin.
func TestControllerTopologyErrorPropagates(t *testing.T) {
	a := &recordingApplier{topoErr: errors.New("no sysfs"), supported: true}
	c := NewController(a)
	err := c.OnGuestReady(ReadyEvent{ForkID: "f1", ThreadIDs: []int{100}, VCPUs: 1, Config: Config{Enabled: true}})
	if err == nil {
		t.Fatal("expected topology error to propagate")
	}
}
