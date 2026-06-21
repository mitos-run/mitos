package fork

import (
	"log"

	"mitos.run/mitos/internal/cpupin"
	"mitos.run/mitos/internal/firecracker"
)

// This file wires the dynamic CPU pinning + launch RT priority hooks (issue
// #168) into the fork path. The decision logic and the scheduler mutation live
// in internal/cpupin (the plan is pure and unit-tested; the apply is Linux-
// gated). Here we only resolve the per-fork config, gather the vCPU thread IDs,
// and call the controller at the two hook points: launch-start (before the
// resume burst) and guest-ready (after resume).
//
// Both hooks are best-effort: pinning is a density/predictability optimization,
// not a correctness requirement, so a failure logs and the fork proceeds
// unpinned. The pin stays within the husk pod cpuset (the topology read is the
// pod's allowed set), so it never regresses the CoW memcg accounting (#33).

// pinController returns the engine's lazily-created cpupin controller, building
// one over the platform applier on first use so engines that never pin pay
// nothing.
func (e *Engine) pinController() *cpupin.Controller {
	e.pinCtlMu.Lock()
	defer e.pinCtlMu.Unlock()
	if e.pinCtl == nil {
		e.pinCtl = cpupin.NewController(cpupin.NewApplier())
	}
	return e.pinCtl
}

// readyEvent builds the cpupin hook payload for a fork, or returns ok=false when
// pinning is disabled (the common path) so the caller skips the hook entirely.
// The vCPU thread IDs come from the VMM process; off Linux that enumeration
// fails and the (no-op) applier would never touch the scheduler anyway, so a
// failure here just yields an empty thread set the no-op stub ignores.
func readyEvent(fcClient *firecracker.Client, opts ForkOpts) (cpupin.ReadyEvent, bool) {
	if opts.CPUPinning == nil || !opts.CPUPinning.Enabled {
		return cpupin.ReadyEvent{}, false
	}
	vcpus := opts.VCPUs
	if vcpus < 1 {
		vcpus = 1
	}
	// Enumerate the Firecracker vCPU threads (Linux-only). A failure leaves the
	// thread set empty; the applier validates and the (no-op off Linux) path skips.
	// A nil client (test path) leaves the thread set empty for the same reason.
	var tids []int
	if fcClient != nil {
		tids, _ = cpupin.VCPUThreadIDs(fcClient.PID())
	}
	return cpupin.ReadyEvent{
		ThreadIDs: tids,
		VCPUs:     vcpus,
		Config:    *opts.CPUPinning,
	}, true
}

// onLaunchStart raises the launch-window scheduling priority before the resume
// burst. Best-effort.
func (e *Engine) onLaunchStart(sandboxID string, fcClient *firecracker.Client, opts ForkOpts) {
	ev, ok := readyEvent(fcClient, opts)
	if !ok {
		return
	}
	ev.ForkID = sandboxID
	if err := e.pinController().OnLaunchStart(ev); err != nil {
		log.Printf("cpupin: launch-priority raise skipped for %s: %v", sandboxID, err)
	}
}

// onGuestReady pins the fork's vCPU threads per the pool policy and drops the
// launch priority, AFTER the guest is resumed. Best-effort.
func (e *Engine) onGuestReady(sandboxID string, fcClient *firecracker.Client, opts ForkOpts) {
	ev, ok := readyEvent(fcClient, opts)
	if !ok {
		return
	}
	ev.ForkID = sandboxID
	if err := e.pinController().OnGuestReady(ev); err != nil {
		log.Printf("cpupin: post-ready pin skipped for %s: %v", sandboxID, err)
	}
}

// forgetPin releases a torn-down fork's pin reservation so its core(s) free up
// for later forks. Safe to call unconditionally; a no-op when the controller was
// never created or the fork was never pinned.
func (e *Engine) forgetPin(sandboxID string) {
	e.pinCtlMu.Lock()
	ctl := e.pinCtl
	e.pinCtlMu.Unlock()
	if ctl != nil {
		ctl.Forget(sandboxID)
	}
}
