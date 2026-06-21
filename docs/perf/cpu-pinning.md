# Dynamic CPU pinning and launch scheduling priority

Tracking: issue #168. Status: DESIGN plus the packing-policy spec field, the
pure pin-plan selection logic, a Linux-gated affinity/RT applier skeleton, the
post-ready hook wired into the fork path, and a bench aggregation. Every density
and activate-success figure in this document is a TARGET until measured on the
bare-metal reference node (#16). Nothing here is a measured claim (CLAUDE.md
operating principle 1).

## The problem

A node packs many forked microVMs onto a fixed set of physical cores. With no
pinning, every fork's vCPU threads float across the whole cpuset and the kernel
scheduler balances them. That is fine for a steady population but two things hurt
under a claim storm:

1. Density is unpredictable. Two tenants' vCPU threads can land on the two
   hyperthread siblings of one physical core, so they contend on shared core
   resources and the per-fork latency varies with whoever else is co-scheduled.
2. Activate loss spikes. During a burst of launches the launching vCPU threads
   compete with everything already running, and some activations miss their
   readiness deadline and fail.

Browser Use reported the shape of this on their stack (we cite it as motivation,
not as a mitos measurement): pinning vCPUs FROM startup actually HURT, because it
serialized the launch burst onto too few cores; the win came from leaving vCPUs
unpinned during the launch burst so the kernel spreads them across cores, THEN
pinning for predictable packing once the guest is ready, assigning BOTH
hyperthread siblings of a physical core to one vCPU, and bumping scheduling
priority to real time DURING launch, which took their launch loss from 17% to 0%.

## The levers

### 1. Pin AFTER guest-ready, never from startup

The launch burst runs unpinned: a fork's vCPU threads float across the husk pod
cpuset while the VM restores and the guest comes up, so the kernel spreads the
burst across cores. Only once the guest is ready does the node pin that fork's
vCPU threads to specific physical core(s). This is the single most important
lesson: pinning from startup serializes the burst and regresses launch.

### 2. Sibling hyperthread pairing

When the node pins a vCPU it pins it to BOTH logical CPUs of a physical core (the
hyperthread siblings), so a vCPU thread and its sibling never serve two different
tenants. On a node with hyperthreading disabled this degrades to one logical CPU
per vCPU.

### 3. Pack vs spread

- `pack` consolidates forks onto as few physical cores as possible, for maximum
  density. No two tenants share a physical core.
- `spread` distributes forks across distinct physical cores, for lower
  contention.

### 4. Launch-window scheduling priority

During the activate window the fork's vCPU threads are bumped to an elevated
scheduling priority, then dropped back after guest-ready, so the launch burst
wins the CPU it needs to hit its readiness deadline without the elevated priority
leaking into steady state.

## The pool spec

`SandboxPool.spec.cpuPinning` (api group `mitos.run/v1alpha1`):

```yaml
spec:
  cpuPinning:
    enabled: true          # default false: legacy unpinned behavior
    policy: pack            # pack | spread; default pack
    siblingPairing: true    # default true
    launchRtPriority: true  # default true
```

Defaults are filled by the API server (kubebuilder markers) and mirrored in Go by
`CPUPinningSpec.Normalized()` for the in-process path. With `enabled: false` (the
default) nothing is pinned and the behavior is exactly as before.

## How it fits the cgroup / cpuset model (does NOT regress #33)

The pin is applied WITHIN the husk pod's cpuset. The topology the planner reads
is the pod's allowed CPU set, so a pinned vCPU thread can only land on a CPU the
pod cgroup already grants. The pin therefore never escapes the pod cgroup and
never changes which cgroup a fork's memory is charged to: the CoW memcg
accounting (#33) is untouched, because affinity (which CPU a thread runs on) is
orthogonal to memory accounting (which memcg its pages are charged to). The husk
pod cgroup model (the pod owns the cpuset and the memcg) is preserved; pinning
only narrows the CPU placement inside that existing boundary.

## Implementation map

- Pure pin-plan selection (`internal/cpupin/plan.go`): given a node topology and
  the set of currently pinned forks, computes which logical CPUs a newly ready
  fork pins to, pairing siblings and honoring pack/spread. Fully unit-tested on
  any platform (no syscalls). The launch burst is modeled as unpinned: only
  Ready, pinned forks reserve a core.
- Topology parse (`internal/cpupin/topology.go`): parses
  `/sys/devices/system/cpu` (online list, `core_id`, `physical_package_id`,
  `thread_siblings_list`) into physical cores with their sibling logical CPUs.
  The parser is pure and fixture-tested; the Linux reader is in
  `topology_linux.go`.
- Linux-gated applier (`internal/cpupin/apply_linux.go`): issues
  `sched_setaffinity` over the pin plan and bumps/drops the launch-window
  priority. The affinity wiring is real; the true `SCHED_FIFO` real-time class
  switch is the gated bare-metal piece (it needs `CAP_SYS_NICE` in the pod and a
  host RT runtime budget), with a nice-level bump applied today. The darwin stub
  (`apply_other.go`) is a no-op.
- Controller (`internal/cpupin/controller.go`): the per-node object the engine
  calls. `OnLaunchStart` raises priority before the resume burst; `OnGuestReady`
  computes the plan, applies the pin, and drops the priority; `Forget` releases a
  torn-down fork's core.
- Hook point (`internal/fork/pinning.go`, `internal/fork/engine.go`): the fork
  path calls `onLaunchStart` immediately before `Resume()` and `onGuestReady`
  immediately after, both no-ops on darwin and for pools that do not opt in. The
  vCPU thread IDs come from the Firecracker process (`fc_vcpu` threads under
  `/proc/<pid>/task`, Linux-only).

## Bench plan (TARGETS until measured on #16)

`cmd/bench --mode pinning` measures activate success rate and activate latency
under a claim storm, pinning ON vs OFF, and reports the success-rate lift. The
aggregation (`benchstat.AggregatePinning`: per-arm success rate + latency
distribution, on-vs-off lift) is pure and unit-tested. The measurement itself
needs Linux + KVM + bare metal + a real claim storm:

- darwin and any non-KVM host: the mode refuses to emit a number (the applier is
  a no-op, so there is nothing to measure) rather than fabricating one.
- bare-metal node (#16): the claim-storm activate-success driver, tied to the
  chaos suite (#163), forks under contention with pinning off then on, records
  each `ActivateOutcome`, and hands both arms to `AggregatePinning`.

Targets to validate on #16 (NOT measured, motivated by the Browser Use numbers):

- activate success rate under a claim storm: pinning ON >= pinning OFF, with the
  gap widening as storm intensity rises (their launch loss went 17% -> 0%).
- higher and more predictable density (tighter activate-latency p99) under pack.

## Remaining bare-metal work

- The `SCHED_FIFO` real-time class switch (currently a nice-level bump).
- The claim-storm bench driver that produces the `ActivateOutcome` samples.
- End-to-end plumbing of `spec.cpuPinning` from the controller through the forkd
  gRPC `Fork` request into `fork.ForkOpts.CPUPinning` (the engine hook and the
  `ForkOpts` surface are in place; the proto/controller wiring is the follow-up).
- Reading the husk pod cpuset (rather than the whole node) as the planner
  topology, so the pin is provably inside the pod cgroup on a shared node.
