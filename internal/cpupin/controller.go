package cpupin

import (
	"fmt"
	"sync"
)

// Config is the per-fork pinning configuration the daemon derives from the
// pool's CPUPinningSpec (api/v1alpha1) and threads to the engine. It is the
// fork-side, dependency-free mirror of the CRD field, so internal/fork and
// internal/cpupin never import the API package.
type Config struct {
	Enabled          bool
	Policy           Policy
	SiblingPairing   bool
	LaunchRtPriority bool
}

// ReadyEvent is the post-guest-ready hook payload for one fork: which fork, its
// Firecracker vCPU thread IDs, its vCPU count, and the pinning config in effect.
// The engine constructs one of these AFTER Resume + guest-ready and hands it to
// the Controller.
type ReadyEvent struct {
	ForkID    string
	ThreadIDs []int
	VCPUs     int
	Config    Config
}

// Controller owns the per-node dynamic-pinning state: which forks are currently
// pinned to which CPUs, so each new ready fork's plan accounts for the others.
// It is the single object the engine calls from its post-ready hook. It is
// concurrency-safe. The actual scheduler mutation is delegated to an Applier,
// which is a no-op off Linux, so the whole flow runs (and is unit-tested) on
// darwin while applying nothing there.
type Controller struct {
	applier Applier
	mu      sync.Mutex
	running map[string]Fork // forkID -> its current pin state
}

// NewController returns a Controller over the given Applier.
func NewController(applier Applier) *Controller {
	return &Controller{applier: applier, running: map[string]Fork{}}
}

// OnGuestReady is the post-ready hook. When pinning is disabled it is a no-op
// (the default). When enabled it: reads the node topology, computes the pin plan
// against the currently-pinned forks (honoring spread/pack and sibling
// pairing), applies the pin to the fork's vCPU threads, records the fork as
// running so later forks plan around it, and drops the launch-window priority.
//
// It is NEVER called during the launch burst: the engine calls it only after
// Resume and guest-ready, so the burst runs unpinned, exactly the Browser Use
// lesson. RaiseLaunchPriority is the separate, earlier hook (OnLaunchStart).
func (c *Controller) OnGuestReady(ev ReadyEvent) error {
	if !ev.Config.Enabled {
		return nil
	}
	if ev.VCPUs < 1 {
		return fmt.Errorf("cpupin: fork %q reported %d vCPUs", ev.ForkID, ev.VCPUs)
	}

	topo, err := c.applier.ReadTopology()
	if err != nil {
		return fmt.Errorf("cpupin: read topology for fork %q: %w", ev.ForkID, err)
	}

	c.mu.Lock()
	running := make([]Fork, 0, len(c.running))
	for _, f := range c.running {
		running = append(running, f)
	}
	c.mu.Unlock()

	plan, err := ComputePinPlan(topo, running, Fork{ID: ev.ForkID, VCPUs: ev.VCPUs, Ready: true}, Options{
		Policy:         ev.Config.Policy,
		SiblingPairing: ev.Config.SiblingPairing,
	})
	if err != nil {
		return fmt.Errorf("cpupin: compute pin plan for fork %q: %w", ev.ForkID, err)
	}
	cpus := plan.CPUsFor(ev.ForkID)

	if err := c.applier.ApplyPin(PinRequest{ThreadIDs: ev.ThreadIDs, CPUs: cpus}); err != nil {
		return fmt.Errorf("cpupin: apply pin for fork %q: %w", ev.ForkID, err)
	}

	c.mu.Lock()
	c.running[ev.ForkID] = Fork{ID: ev.ForkID, VCPUs: ev.VCPUs, Ready: true, PinnedCPUs: cpus}
	c.mu.Unlock()

	// Drop the launch-window priority now that the guest is ready, so the elevated
	// priority covered only the activate burst.
	if ev.Config.LaunchRtPriority {
		if err := c.applier.DropLaunchPriority(ev.ThreadIDs); err != nil {
			return fmt.Errorf("cpupin: drop launch priority for fork %q: %w", ev.ForkID, err)
		}
	}
	return nil
}

// OnLaunchStart is the activate-window hook: it bumps the fork's vCPU threads to
// the launch scheduling priority BEFORE the burst, to cut launch loss. It is a
// no-op when pinning or the RT bump is disabled. The matching drop happens in
// OnGuestReady after the guest is ready.
func (c *Controller) OnLaunchStart(ev ReadyEvent) error {
	if !ev.Config.Enabled || !ev.Config.LaunchRtPriority {
		return nil
	}
	if err := c.applier.RaiseLaunchPriority(ev.ThreadIDs); err != nil {
		return fmt.Errorf("cpupin: raise launch priority for fork %q: %w", ev.ForkID, err)
	}
	return nil
}

// Forget releases a fork's pin reservation when it is torn down, so its
// physical core(s) become available to later forks.
func (c *Controller) Forget(forkID string) {
	c.mu.Lock()
	delete(c.running, forkID)
	c.mu.Unlock()
}
