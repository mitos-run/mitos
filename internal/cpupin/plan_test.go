package cpupin

import (
	"reflect"
	"testing"
)

// sharesPhysicalCore reports whether any cpu in a and any cpu in b map to the
// same physical core under topo.
func sharesPhysicalCore(topo Topology, a, b []int) bool {
	cpuToCore := map[int]int{}
	for _, c := range topo.Cores {
		for _, l := range c.Logical {
			cpuToCore[l] = c.ID
		}
	}
	bcores := map[int]bool{}
	for _, l := range b {
		bcores[cpuToCore[l]] = true
	}
	for _, l := range a {
		if bcores[cpuToCore[l]] {
			return true
		}
	}
	return false
}

// fixtureTopology is a 4-physical-core, hyperthreaded node: 8 logical CPUs,
// each physical core owning two sibling logical CPUs. This is the canonical
// "core 0 owns CPUs {0,4}" Linux layout where sibling N pairs with N + ncores.
func fixtureTopology() Topology {
	return Topology{Cores: []PhysicalCore{
		{ID: 0, Logical: []int{0, 4}},
		{ID: 1, Logical: []int{1, 5}},
		{ID: 2, Logical: []int{2, 6}},
		{ID: 3, Logical: []int{3, 7}},
	}}
}

// TestPlanSiblingPairing proves that with sibling pairing on, a single-vCPU
// fork pins to BOTH logical siblings of one physical core, never a lone
// hyperthread.
func TestPlanSiblingPairing(t *testing.T) {
	topo := fixtureTopology()
	plan, err := ComputePinPlan(topo, nil, Fork{ID: "f1", VCPUs: 1}, Options{
		Policy:         PolicyPack,
		SiblingPairing: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	got := plan.CPUsFor("f1")
	want := []int{0, 4}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("vCPU0 cpus = %v, want %v (both siblings of core 0)", got, want)
	}
}

// TestPlanNoSiblingPairing proves that with pairing off, a single-vCPU fork
// pins to exactly one logical CPU.
func TestPlanNoSiblingPairing(t *testing.T) {
	topo := fixtureTopology()
	plan, err := ComputePinPlan(topo, nil, Fork{ID: "f1", VCPUs: 1}, Options{
		Policy:         PolicyPack,
		SiblingPairing: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	got := plan.CPUsFor("f1")
	if len(got) != 1 {
		t.Fatalf("with pairing off a 1-vCPU fork must pin to 1 cpu, got %v", got)
	}
}

// TestPlanPackNoCoreSharing proves PACK never lets two DIFFERENT forks share a
// physical core: each fork lands on its own core(s).
func TestPlanPackNoCoreSharing(t *testing.T) {
	topo := fixtureTopology()
	// f1 is placed first.
	plan1, err := ComputePinPlan(topo, nil, Fork{ID: "f1", VCPUs: 1}, Options{Policy: PolicyPack, SiblingPairing: true})
	if err != nil {
		t.Fatal(err)
	}
	c1 := plan1.CPUsFor("f1")
	// f1 is now READY and pinned; f2 placed with f1 reserving its core.
	f1Running := Fork{ID: "f1", VCPUs: 1, Ready: true, PinnedCPUs: c1}
	plan2, err := ComputePinPlan(topo, []Fork{f1Running}, Fork{ID: "f2", VCPUs: 1}, Options{Policy: PolicyPack, SiblingPairing: true})
	if err != nil {
		t.Fatal(err)
	}
	c2 := plan2.CPUsFor("f2")
	if sharesPhysicalCore(topo, c1, c2) {
		t.Fatalf("PACK let f1 (%v) and f2 (%v) share a physical core", c1, c2)
	}
	// PACK fills core 0 then core 1, so f1->core0, f2->core1.
	if !reflect.DeepEqual(c1, []int{0, 4}) || !reflect.DeepEqual(c2, []int{1, 5}) {
		t.Fatalf("PACK placement unexpected: f1=%v f2=%v", c1, c2)
	}
}

// TestPlanSpreadDistributes proves SPREAD pushes a new fork to the LEAST loaded
// physical core, distributing rather than packing.
func TestPlanSpreadDistributes(t *testing.T) {
	topo := fixtureTopology()
	// f1 already on core 0.
	running := []Fork{{ID: "f1", VCPUs: 1, Ready: true, PinnedCPUs: []int{0, 4}}}
	plan, err := ComputePinPlan(topo, running, Fork{ID: "f2", VCPUs: 1}, Options{
		Policy:         PolicySpread,
		SiblingPairing: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	c2 := plan.CPUsFor("f2")
	// SPREAD should pick a DIFFERENT, empty core. Core 1 is the next least loaded.
	if sharesPhysicalCore(topo, []int{0, 4}, c2) {
		t.Fatalf("SPREAD placed f2 (%v) on the same core as f1", c2)
	}
}

// TestPlanMultiVCPU proves a multi-vCPU fork gets one core per vCPU under pack
// with sibling pairing.
func TestPlanMultiVCPU(t *testing.T) {
	topo := fixtureTopology()
	plan, err := ComputePinPlan(topo, nil, Fork{ID: "f1", VCPUs: 2}, Options{
		Policy:         PolicyPack,
		SiblingPairing: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	got := plan.CPUsFor("f1")
	want := []int{0, 4, 1, 5}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("2-vCPU fork cpus = %v, want %v", got, want)
	}
}

// TestPlanLaunchBurstUnpinned proves the launch burst is NOT pinned: the plan
// only ever covers the post-ready fork, and a fork that is not yet ready
// (Ready=false) is excluded from the running set the planner accounts for.
func TestPlanLaunchBurstUnpinned(t *testing.T) {
	topo := fixtureTopology()
	// f1 is mid-launch (not ready) so it holds no pin and does not reserve a core.
	running := []Fork{{ID: "f1", VCPUs: 1, Ready: false}}
	plan, err := ComputePinPlan(topo, running, Fork{ID: "f2", VCPUs: 1}, Options{
		Policy:         PolicyPack,
		SiblingPairing: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Because f1 is unpinned (launch burst), f2 packs onto core 0 unobstructed.
	if got := plan.CPUsFor("f2"); !reflect.DeepEqual(got, []int{0, 4}) {
		t.Fatalf("unready launch-burst fork must not reserve a core; f2=%v", got)
	}
}

// TestPlanExhaustion proves the planner returns an error (never a partial or
// fabricated plan) when there is no free physical core under pack.
func TestPlanExhaustion(t *testing.T) {
	topo := Topology{Cores: []PhysicalCore{{ID: 0, Logical: []int{0, 1}}}}
	running := []Fork{{ID: "f1", VCPUs: 1, Ready: true, PinnedCPUs: []int{0, 1}}}
	_, err := ComputePinPlan(topo, running, Fork{ID: "f2", VCPUs: 1}, Options{
		Policy:         PolicyPack,
		SiblingPairing: true,
	})
	if err == nil {
		t.Fatal("expected an exhaustion error when no free core remains under pack")
	}
}
