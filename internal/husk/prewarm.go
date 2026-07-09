package husk

import (
	"context"
	"fmt"
	"os"
)

// Pre-warmed co-located child slot (perf: cut the fork spawn latency).
//
// A co-located fork pays a per-fork process boot (fc_boot) plus the dormant
// prepare overhead on the fork HOT PATH because the child Firecracker is prepared
// ON DEMAND at fork time (prepareInstance -> boot -> activate). This file keeps
// ONE dormant, GENERIC child Firecracker pre-prepared (booted, and template-
// snapshot-verified when a template is configured) in a multi-VM pod, so a fork
// ADOPTS the ready child (SpawnVM -> prepareInstanceOpt{reuseVM} -> activate)
// instead of booting one, moving fc_boot and the verify off the fork hot path.
//
// SAFE FOR THE LIVE-COW LAZY-UFFD PATH: the child Firecracker is booted GENERIC
// (no fork-specific launch env). The fork-specific part, the live-cow lazy-UFFD
// import, is armed on the dormant instance AFTER prepare and BEFORE activate
// (armInstanceChildUFFD in SpawnVM), reading the source's FROZEN composite through
// the husk fault handler at LoadSnapshotUFFD time. The pre-warmed child reaches
// activate in the SAME StateDormant, so it arms and restores through the husk
// UFFD fault handler identically; the pre-warm only changes WHEN the process was
// booted, never HOW the child restores or inherits, so the no-leak invariant is
// untouched. The per-fork rootfs clone stays at fork time (the child needs the
// source's fork-time rootfs the fork snapshot carries), so the pre-warmed slot is
// a generic boot only.
//
// It is gated behind Options.PrewarmChild (default OFF) and needs MultiVM. At
// most ONE dormant child is kept (single-flight re-warm), so the pre-warm never
// over-admits the pod's per-VM memory budget: a dormant child counts as one extra
// VM against co-location capacity, exactly as an on-demand child would once
// spawned, just paid a fork earlier.

// prewarmSlotVMID is the reserved instances-map key the pre-warmed dormant child
// is prepared under. It satisfies validVMID (leading alphanumeric) and is namesp
// aced so a controller-assigned fork vmID does not collide with it. It is NEVER
// the vmID a fork activates under: SpawnVM ADOPTS the slot's booted Firecracker
// into the fork's OWN vmID instance, so the slot is a boot reservation, not a
// tenant VM (a dormant slot is not metered, meteringMulti counts StateActive
// only).
const prewarmSlotVMID vmID = "huskprewarmchild0"

// PrewarmChild eagerly prepares the pod's dormant child slot so the FIRST
// co-located fork already skips the process boot, instead of warming lazily after
// the first fork. A pod lifecycle can call it once the source (default) VM is
// active. It is a no-op (nil) when pre-warm is off or the slot is already warm,
// and returns the prepare error (fail-closed) when the boot itself fails, so a
// caller can log it without the fork path ever depending on it. Safe to call
// repeatedly.
func (s *Stub) PrewarmChild(ctx context.Context) error {
	// FAIL CLOSED on the flag, exactly like SpawnVM: a single-VM pod owns exactly
	// one VM and has a nil instances map, so pre-warming a second dormant child
	// there is refused with a clear error rather than touching the nil map.
	if !s.multiVM {
		return fmt.Errorf("husk: pre-warm child refused: this pod is not running in multi-VM mode")
	}
	return s.warmPrewarmChild(ctx)
}

// consumePrewarmedChild pops the pre-warmed dormant child's booted Firecracker for
// adoption into a fork's instance, returning nil on a miss (pre-warm off, no slot,
// or the slot not dormant). It scopes s.mu to the map lookup, then takes ONLY the
// slot instance's own lock to detach the VMM and reset the slot to StateNew so a
// re-warm can refill it. The caller adopts the returned VMM via
// prepareInstanceOpt{reuseVM}; on a miss the caller boots on demand. It never
// touches a sibling instance.
func (s *Stub) consumePrewarmedChild() vmm {
	if !s.multiVM || !s.prewarmChild {
		return nil
	}
	inst := s.instanceFor(prewarmSlotVMID, false)
	if inst == nil {
		return nil
	}
	inst.mu.Lock()
	defer inst.mu.Unlock()
	if inst.state != StateDormant || inst.vm == nil {
		return nil
	}
	vm := inst.vm
	inst.vm = nil
	// A generic pre-warmed boot never armed a per-fork import or per-activation
	// artifact, but reset defensively so the freed slot re-warms from a clean state.
	inst.childUFFDPlan = nil
	inst.rootfsClonePath = ""
	inst.prepareVerified = false
	inst.state = StateNew
	return vm
}

// warmPrewarmChild prepares (boots, and verifies the template snapshot when
// configured) ONE dormant, GENERIC child Firecracker under the reserved slot, if
// pre-warm is on and no re-warm is already running. It is single-flight (s.prewarming)
// so the pod never keeps more than one dormant child. It skips the per-fork rootfs
// clone (skipRootfsClone): the fork clones its own rootfs from the fork snapshot at
// consume. Fail-closed: on any error the slot is left out of StateDormant (a later
// fork simply boots on demand). It never blocks a fork; SpawnVM runs it in the
// background via rewarmPrewarmChildAsync.
func (s *Stub) warmPrewarmChild(ctx context.Context) error {
	// Guard on multiVM too: the instances map is nil on a single-VM stub, so
	// prepareInstanceOpt (which inserts into it) must never run there.
	if !s.multiVM || !s.prewarmChild {
		return nil
	}
	// Single-flight + closing guard under s.mu: claim the re-warm, or bail if one is
	// already running, the pod is closing, or the slot is already dormant.
	s.mu.Lock()
	if s.closing || s.prewarming {
		s.mu.Unlock()
		return nil
	}
	if inst := s.instances[prewarmSlotVMID]; inst != nil {
		inst.mu.Lock()
		warm := inst.state == StateDormant
		inst.mu.Unlock()
		if warm {
			s.mu.Unlock()
			return nil
		}
	}
	s.prewarming = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.prewarming = false
		s.mu.Unlock()
	}()

	if err := s.prepareInstanceOpt(ctx, prewarmSlotVMID, prepareOpts{skipRootfsClone: true}); err != nil {
		return fmt.Errorf("husk: pre-warm dormant child: %w", err)
	}
	return nil
}

// rewarmPrewarmChildAsync replenishes the pre-warmed slot for the NEXT fork off
// the current fork's hot path. It is a no-op when pre-warm is off; otherwise it
// runs warmPrewarmChild on a background context (the fork's ctx may be cancelled
// once SpawnVM returns) and logs, never returns, the re-warm error so a fork never
// waits on or fails from the pool refill. single-flight in warmPrewarmChild keeps
// concurrent calls to one dormant child.
func (s *Stub) rewarmPrewarmChildAsync() {
	s.warmPrewarmChildAsync("re-warm pre-warmed child failed (next fork boots on demand)")
}

// eagerPrewarmChildAsync boots the pod's dormant child slot as soon as the SOURCE
// (default) VM has activated, so a pod's FIRST co-located fork already finds a warm
// slot to ADOPT (fc_boot=0) instead of paying the process boot on its hot path.
// Without this the slot is created ONLY by the post-fork rewarmPrewarmChildAsync,
// so the FIRST fork of a freshly claimed pod always misses the slot (in prod most
// forks are a fresh pod's first fork). activateInstance calls it once the default
// VM reaches StateActive; it is a no-op when pre-warm is off or the pod is
// single-VM. It runs OFF the activate path (its own goroutine + a background
// context, since the activate ctx may be cancelled once Activate returns) so it
// never delays the activation or the first request. warmPrewarmChild is
// single-flight and fail-closed, so an eager warm racing the first fork is safe: a
// miss just falls back to the on-demand prepare, and the pod never keeps more than
// one dormant child. It fires ONLY on the source activation (a CLAIMED pod), so an
// unclaimed warm-pool pod never boots a dormant child it may never fork, and the
// warm slot the fork adopts is the SAME extra VM the on-demand fork would have
// booted, so eager warming never over-admits the pod's per-VM memory budget.
func (s *Stub) eagerPrewarmChildAsync() {
	s.warmPrewarmChildAsync("eager pre-warm child at source activation failed (first fork boots on demand)")
}

// warmPrewarmChildAsync is the shared body of the eager (source-activation) and
// re-warm (post-fork) paths: a no-op when pre-warm is off or the pod is single-VM,
// else it runs the single-flight, fail-closed warmPrewarmChild on its own goroutine
// and a background context (the caller's ctx may be cancelled once it returns),
// logging failCtx on error so neither path ever blocks or fails on the pool refill.
func (s *Stub) warmPrewarmChildAsync(failCtx string) {
	if !s.multiVM || !s.prewarmChild {
		return
	}
	go func() {
		if err := s.warmPrewarmChild(context.Background()); err != nil {
			fmt.Fprintf(os.Stderr, "husk: %s: %v\n", failCtx, err)
		}
	}()
}
