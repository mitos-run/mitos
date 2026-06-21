//go:build linux

package cpupin

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// This file is the Linux-gated applier for dynamic CPU pinning and the
// launch-time scheduling-priority bump (issue #168). It compiles under
// GOOS=linux and defines the real shape of the apply path. The affinity wiring
// (sched_setaffinity over a CPU mask) is real here. The true real-time class
// switch (SCHED_FIFO via sched_setscheduler) is the bare-metal piece: it needs
// CAP_SYS_NICE inside the husk pod and a host where an RT runtime budget is
// configured, so it is gated behind raiseRealtime/dropRealtime below, which
// today apply a nice-level bump (always permitted) and leave the SCHED_FIFO
// switch as a localized, well-scoped follow-up. The structure, the post-ready
// hook points, and the raise-then-drop lifecycle are all present and correct.
//
// Everything here stays WITHIN the husk pod cpuset: the CPUs come from a PinPlan
// computed over a Topology read from the pod's allowed CPU set, so the pin never
// escapes the pod cgroup and never regresses the CoW memcg accounting (#33).

// launchNiceDelta is the nice-level decrease applied to vCPU threads during the
// launch window. A lower nice value is a higher scheduling priority. This is the
// always-permitted portion of the launch bump; the SCHED_FIFO switch is gated.
const launchNiceDelta = -10

// linuxApplier issues sched_setaffinity and the launch-priority bump against
// Firecracker vCPU thread IDs.
type linuxApplier struct{}

// NewApplier returns the real Linux applier.
func NewApplier() Applier { return linuxApplier{} }

// ReadTopology reads the node (or husk pod cpuset) topology from
// /sys/devices/system/cpu via the Linux sysfs reader.
func (linuxApplier) ReadTopology() (Topology, error) {
	return parseTopology(linuxSysfs{})
}

// ApplyPin pins each of the request's vCPU threads to the request's CPU set via
// sched_setaffinity. Called AFTER guest-ready. Fail closed: a single thread that
// cannot be pinned aborts the apply so the caller never believes a partial pin
// succeeded.
func (linuxApplier) ApplyPin(req PinRequest) error {
	if err := req.validate(); err != nil {
		return err
	}
	var set unix.CPUSet
	set.Zero()
	for _, c := range req.CPUs {
		set.Set(c)
	}
	for _, tid := range req.ThreadIDs {
		if err := unix.SchedSetaffinity(tid, &set); err != nil {
			return fmt.Errorf("cpupin: sched_setaffinity tid=%d cpus=%v: %w", tid, req.CPUs, err)
		}
	}
	return nil
}

// RaiseLaunchPriority bumps the given vCPU threads to the launch-window
// priority. Called as the activate window opens, BEFORE the burst.
func (linuxApplier) RaiseLaunchPriority(threadIDs []int) error {
	for _, tid := range threadIDs {
		if err := raiseRealtime(tid); err != nil {
			return fmt.Errorf("cpupin: raise launch priority tid=%d: %w", tid, err)
		}
	}
	return nil
}

// DropLaunchPriority restores the given vCPU threads to the normal scheduling
// class. Called AFTER guest-ready, so the elevated priority covers only launch.
func (linuxApplier) DropLaunchPriority(threadIDs []int) error {
	for _, tid := range threadIDs {
		if err := dropRealtime(tid); err != nil {
			return fmt.Errorf("cpupin: drop launch priority tid=%d: %w", tid, err)
		}
	}
	return nil
}

// Supported reports true: this applier changes scheduler state.
func (linuxApplier) Supported() bool { return true }

// raiseRealtime elevates one thread's scheduling priority for the launch window.
// The always-permitted nice-level bump is applied here. The SCHED_FIFO switch
// (sched_setscheduler with an RT priority) is the gated bare-metal follow-up: it
// requires CAP_SYS_NICE in the husk pod and a host RT runtime budget, so it is
// intentionally left as a localized addition rather than fabricated here.
func raiseRealtime(tid int) error {
	if err := unix.Setpriority(unix.PRIO_PROCESS, tid, launchNiceDelta); err != nil {
		return fmt.Errorf("setpriority(nice=%d): %w", launchNiceDelta, err)
	}
	return nil
}

// dropRealtime restores one thread to the default nice level (0). When the gated
// SCHED_FIFO switch lands, this is also where the thread returns to SCHED_OTHER.
func dropRealtime(tid int) error {
	if err := unix.Setpriority(unix.PRIO_PROCESS, tid, 0); err != nil {
		return fmt.Errorf("setpriority(nice=0): %w", err)
	}
	return nil
}
