package cpupin

import "fmt"

// PinRequest is one fork's pin instruction: pin these Firecracker vCPU thread
// IDs to these logical CPUs. CPUs comes straight from a PinPlan's CPUsFor.
type PinRequest struct {
	// ThreadIDs are the OS thread IDs (tids) of the Firecracker vCPU threads to
	// pin. On Linux these are the per-vCPU kernel threads of the FC process.
	ThreadIDs []int
	// CPUs is the logical CPU set the threads are pinned to (the PinPlan output).
	CPUs []int
}

func (r PinRequest) validate() error {
	if len(r.ThreadIDs) == 0 {
		return fmt.Errorf("cpupin: PinRequest has no thread IDs")
	}
	if len(r.CPUs) == 0 {
		return fmt.Errorf("cpupin: PinRequest has no CPUs (refusing to set an empty/unconstrained affinity mask)")
	}
	return nil
}

// Applier applies a pin plan and the launch-time scheduling-priority bump to a
// fork's Firecracker vCPU threads. It is the ONLY seam that touches the OS
// scheduler; the plan that drives it is pure (ComputePinPlan). The Linux
// implementation issues sched_setaffinity / sched_setscheduler; every other
// platform gets a no-op stub, so the activate/fork path compiles and runs on
// darwin without applying anything.
//
// The launch-priority lifecycle is explicit and must be honored by callers:
// RaiseLaunchPriority is called as the fork's vCPU threads start (the activate
// window), and DropLaunchPriority is called AFTER guest-ready, so the elevated
// priority covers only the launch burst and is released before steady state.
// ApplyPin is also called AFTER guest-ready (never from startup): pinning from
// startup hurt in the Browser Use measurements, so the launch burst runs
// unpinned and pinning lands only once the guest is ready.
type Applier interface {
	// ReadTopology reads the node's physical-core topology (the husk pod cpuset on
	// a cgroup-constrained host). Unavailable off Linux.
	ReadTopology() (Topology, error)
	// ApplyPin pins the request's threads to its CPUs. Called post-ready.
	ApplyPin(req PinRequest) error
	// RaiseLaunchPriority bumps the given threads to the launch-window scheduling
	// priority. Called as the activate window opens.
	RaiseLaunchPriority(threadIDs []int) error
	// DropLaunchPriority restores the given threads to the normal scheduling
	// class. Called AFTER guest-ready.
	DropLaunchPriority(threadIDs []int) error
	// Supported reports whether this applier actually changes scheduler state
	// (true on Linux, false for the no-op stub). Callers log accordingly.
	Supported() bool
}
