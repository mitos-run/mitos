// Package cpupin computes dynamic (post-ready) CPU pin plans for sandbox VMs
// (issue #168). It is pure topology math: given a node's CPU topology and the
// set of currently pinned forks, it decides which physical core(s) a newly
// ready fork's vCPU threads pin to, pairing hyperthread siblings per vCPU and
// honoring a spread or pack packing policy. It performs NO syscalls and has no
// platform dependency, so the placement logic is fully unit-testable on any
// host; the Linux-gated applier (apply_linux.go) consumes a PinPlan from this
// package and is the only piece that touches sched_setaffinity.
//
// Two lessons from the Browser Use work are encoded here:
//   - The launch burst is left UNPINNED: only forks that are Ready hold a pin
//     and reserve a core, so an in-flight fork floats across the husk pod cpuset
//     during its launch and is pinned only after guest-ready.
//   - Both hyperthread siblings of a physical core are assigned to one vCPU
//     (SiblingPairing), so a vCPU thread and its sibling never serve two tenants.
//
// The plan stays WITHIN the husk pod's cpuset: the Topology handed in is the
// pod's allowed CPU set, so the pin never escapes the pod cgroup and never
// regresses the CoW memcg accounting (issue #33).
package cpupin

import (
	"fmt"
	"sort"
)

// Policy is the packing strategy for the pin plan.
type Policy string

const (
	// PolicyPack consolidates forks onto the lowest-numbered free physical cores,
	// for maximum density. No two tenants share a physical core.
	PolicyPack Policy = "pack"
	// PolicySpread distributes forks across distinct physical cores, for lower
	// contention.
	PolicySpread Policy = "spread"
)

// PhysicalCore is one physical core and the logical CPUs (hyperthread siblings)
// it owns. On a non-hyperthreaded node Logical has a single entry.
type PhysicalCore struct {
	ID      int
	Logical []int
}

// Topology is a node's (or husk pod cpuset's) physical-core layout. It is the
// set of cores the planner may place forks on; passing the pod's cpuset keeps
// every pin inside the pod cgroup.
type Topology struct {
	Cores []PhysicalCore
}

// Fork describes a sandbox VM for planning. PinnedCPUs is the logical CPU set a
// already-pinned fork currently occupies (empty for an unpinned, mid-launch
// fork). Ready is false during the launch burst; an unready fork holds no pin
// and reserves no core, so the burst stays unpinned and spread by the kernel.
type Fork struct {
	ID         string
	VCPUs      int
	PinnedCPUs []int
	Ready      bool
}

// Options parameterizes a plan computation.
type Options struct {
	Policy         Policy
	SiblingPairing bool
}

// PinPlan maps a fork ID to the ordered list of logical CPUs its vCPU threads
// pin to. CPUsFor returns the list for a fork.
type PinPlan struct {
	byFork map[string][]int
}

// CPUsFor returns the logical CPU pin set for forkID, or nil if absent.
func (p PinPlan) CPUsFor(forkID string) []int {
	if p.byFork == nil {
		return nil
	}
	return p.byFork[forkID]
}

// ComputePinPlan computes the post-ready pin plan for newFork, given the node's
// topology and the running forks already accounted for. Only forks that are
// Ready and hold a pin reserve a physical core; mid-launch (unready) forks are
// ignored, which is how the launch burst is left unpinned. The plan pairs both
// hyperthread siblings per vCPU when SiblingPairing is set. It returns an error
// (never a partial or fabricated plan) when the topology cannot satisfy the
// fork under the no-core-sharing rule.
func ComputePinPlan(topo Topology, running []Fork, newFork Fork, opts Options) (PinPlan, error) {
	if newFork.VCPUs < 1 {
		return PinPlan{}, fmt.Errorf("cpupin: fork %q has %d vCPUs, want at least 1", newFork.ID, newFork.VCPUs)
	}

	// A physical core is reserved if any READY, pinned fork occupies one of its
	// logical CPUs. Unready (launch-burst) forks reserve nothing.
	reserved := reservedCores(topo, running)

	// Candidate cores are the free ones. PACK takes them in ascending ID order so
	// forks consolidate onto low cores; SPREAD takes the same free set but, by
	// always preferring the least-loaded core (free cores have zero load), also
	// lands on a distinct core, distributing rather than stacking. With the
	// no-core-sharing invariant both reduce to "pick free cores", differing only
	// in ordering, which is the honest darwin-testable shape of the policy.
	free := make([]PhysicalCore, 0, len(topo.Cores))
	for _, c := range topo.Cores {
		if !reserved[c.ID] {
			free = append(free, c)
		}
	}
	orderCores(free, opts.Policy)

	need := newFork.VCPUs
	if len(free) < need {
		return PinPlan{}, fmt.Errorf(
			"cpupin: cannot place fork %q: needs %d physical core(s) under %s but only %d free (topology has %d cores, %d reserved)",
			newFork.ID, need, opts.Policy, len(free), len(topo.Cores), len(topo.Cores)-len(free),
		)
	}

	cpus := make([]int, 0, need*2)
	for i := 0; i < need; i++ {
		core := free[i]
		if opts.SiblingPairing {
			cpus = append(cpus, core.Logical...)
		} else {
			cpus = append(cpus, core.Logical[0])
		}
	}

	return PinPlan{byFork: map[string][]int{newFork.ID: cpus}}, nil
}

// reservedCores returns the set of physical core IDs occupied by a READY,
// pinned fork. Unready forks (the launch burst) reserve nothing.
func reservedCores(topo Topology, running []Fork) map[int]bool {
	cpuToCore := make(map[int]int)
	for _, c := range topo.Cores {
		for _, lcpu := range c.Logical {
			cpuToCore[lcpu] = c.ID
		}
	}
	reserved := make(map[int]bool)
	for _, f := range running {
		if !f.Ready || len(f.PinnedCPUs) == 0 {
			continue
		}
		for _, lcpu := range f.PinnedCPUs {
			if coreID, ok := cpuToCore[lcpu]; ok {
				reserved[coreID] = true
			}
		}
	}
	return reserved
}

// orderCores sorts free cores for selection. Both policies sort by ascending
// core ID (the no-core-sharing invariant makes free-core load uniformly zero);
// PACK consolidates onto the lowest cores, SPREAD distributes by taking the
// next distinct core. Kept as a seam so a future load-aware SPREAD (when
// cross-tenant sharing is allowed) changes only this function.
func orderCores(cores []PhysicalCore, policy Policy) {
	sort.Slice(cores, func(i, j int) bool { return cores[i].ID < cores[j].ID })
}
