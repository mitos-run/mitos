# The winning fork primitive: cheap CoW forks per tenant, on multi-node k8s

Status: design, needs sign-off before implementation. Owner: TBD.
Relationship: supersedes the LATENCY approach in the warm-husk plan
(2026-07-06-warm-husk-live-fork.md, #758). That plan's correctness milestones
(#759 parent-resume, #760 child isolation) still stand and ship first. Its #761
warm-husk-1:1 latency milestone is superseded by this document.

## The one-sentence problem

Mitos already ships a fork primitive that costs about 3 MiB per fork, and the
hosted service does not run it. The husk-pod deployment model (one pod per VM,
one honest 512Mi memory request each) discards a CoW memory-sharing primitive the
raw-forkd engine already has, which is why hosted forks cost 512 MiB and about
4.7s each while a competitor (deeplethe/forkd) does 100 forks in 101ms at 0.12 MiB
per child.

## What we already have (verified, origin/main)

- Real CoW fan-out. Every fork of a template points Firecracker at the SAME
  on-disk snapshot mem file; Firecracker `mmap(MAP_PRIVATE)`s it, so siblings
  share clean pages through the kernel page cache and each fork's resident cost is
  its private-dirty set. Measured about 3 MiB per fork, shared set counted once
  (`internal/fork/engine.go:1209-1236`, `BENCHMARKS.md:72-77,169`).
- A real, wired userfaultfd handler: fault to file-offset arithmetic, hot-page
  capture, and preload (`internal/fork/uffd.go`, `uffd_linux.go`, `uffd_engine.go`).
  It is a read-fault fill + prefetch handler today, wired for hugepage and
  hot-set templates.
- Many VMs per node already, in the raw-forkd engine: forkd is a DaemonSet, one
  Firecracker PROCESS per VM (not one POD per VM). The 512Mi-per-VM cost is a
  property of the husk-pod model layered on top for isolation, not of the engine.
- Stock upstream Firecracker v1.15.0 (not vendored), giving two memory backends:
  File (the `MAP_PRIVATE` CoW path) and Uffd (the read-fault handler).

## What we lack vs deeplethe/forkd

1. The cheap primitive is not on the hosted path. Hosted uses husk-pod
   (one pod per VM), so a fork swarm is N pods x 512Mi, not N x 3 MiB in shared
   memory. This is a deployment-model gap, not an engine gap.
2. Cheap LIVE fork. A live fork (forking a running, mutated parent) writes a FULL
   ~512Mi snapshot today (`SnapshotType: "Full"`, `internal/firecracker/client.go:461-464`),
   then the child `MAP_PRIVATE`s that. Cheap for cold template fan-out, not for
   live parents. Closing this needs a shared/memfd live-parent backing so the
   child CoWs the running parent's pages directly.
3. Async source resume. `ForkRunning` pauses the source across the full snapshot
   write AND the child boot, resuming only in a deferred call after the child is
   up. deeplethe uses UFFD write-protect so the source resumes in about 56ms while
   dirty pages copy in the background. Mitos has no `UFFDIO_WRITEPROTECT` yet, but
   the fault machinery to build it is about 90% present in the existing uffd code.
4. Cross-node fork. Node-local today (checkpoint + rootfs are node-local
   hostPaths, CAS is node-local). No competitor has cross-node fork either.

## Target architecture

### Layer 1: run the CoW engine per tenant, in one pod (the density win)

The husk-pod model gives one pod per VM for isolation. But the pod is NOT the
cross-tenant boundary today (nodes, `/dev/kvm`, snapshots, CAS are shared per node
per ADR 0004; the microVM is the isolation boundary). And fork children are the
SAME tenant as the source. So run a user's fork swarm as MANY same-tenant VMs in
ONE pod, using the raw-forkd CoW engine scoped to that pod:

- A session/fork-tree gets one pod (its own unprivileged, drop-ALL, seccomp,
  netns, cgroup, NetworkPolicy envelope, exactly today's husk-pod posture).
- Inside it, the CoW engine forks N sibling microVMs that `MAP_PRIVATE`-share the
  parent memory image: about 3 MiB per fork, spawned as a control-op (Firecracker
  process spawn + rootfs reflink + reseed handshake, tens of ms), NOT a pod op.
- The N siblings keep microVM isolation (separate KVM + guest kernel), per-VM CoW
  rootfs, and the per-VM fail-closed RNG/clock reseed (`NotifyForked`).
- What is shared within the pod (acceptable because same-tenant): the pod memcg
  (a sibling can OOM its siblings, the user's own workload), the pod netns + one
  egress filter, and the husk-stub blast radius.

This is the largest change: the single-VM husk stub becomes a multi-VM manager
(`internal/husk/stub.go` single `state`/`vm` fields to a `map[vmID]*vmInstance`;
`cmd/husk-stub/main.go` single socket/vmID/workdir to per-VM; controller
`buildForkChildPod` from minting a pod to sending a spawn-VM control-op to the
session pod). Roughly the scope of the original husk-pod work. Gate behind a flag;
keep one-pod-per-VM as the conservative default until proven.

### Layer 2: cheap live fork (memfd backing + UFFD_WP async resume)

To make a live fork of a running parent as cheap as template fan-out:

- Back the parent's guest RAM so a child can CoW the RUNNING parent's pages instead
  of a full snapshot write. Two routes: (a) a shared/memfd Firecracker memory
  backend, which likely needs a patched Firecracker (the one genuinely vendored
  piece, matching what deeplethe did); (b) a UFFD-served live-memory scheme that
  reuses the existing handler to serve the parent's pages to children without a
  full snapshot. Prefer (b) first since it reuses shipped code and avoids vendoring;
  fall back to (a) if (b) cannot hit the latency.
- Add `UFFDIO_WRITEPROTECT` so the source resumes asynchronously while dirty pages
  are captured in the background. Reuses about 90% of the existing uffd fault
  machinery; the net-new is the WP ioctl wiring and the background drain lifecycle.

### Layer 3: cross-node fork (the capability forkd cannot match)

FEASIBLE-EXPENSIVE. Same-node cheapness comes from a resident shared page cache;
across a node boundary every touched page must cross the network exactly once, so
cross-node fork is latency-cheap (fast resume) but never bytes-cheap (about 0.12
MiB is impossible off-node). Minimal v1, same-rack post-copy over UFFD:

1. Source forkd keeps the paused snapshot mem file mmap'd (the uffd handler already
   does) and exposes a gRPC `GetPage(offset, len)` over it: a memory-page server.
2. Target forkd boots the child via the Uffd backend pointed at a local socket
   whose handler, on each fault, computes the mem-file offset (`fileOffsetForAddr`,
   already built) and fetches that page from the source over gRPC instead of a
   local mmap, then `UFFDIO_COPY`s it.
3. Preload the manifest hot-page set (`CaptureTemplateHotPages` to `Preload`,
   already built) OVER THE WIRE before resume to shrink the fault tail.
4. Keep same-rack to bound RTT; reset child network via the existing egress proxy
   (#336). Source stays paused until the working set drains (or, with Layer 2's
   UFFD_WP, resumes asynchronously during the drain).

Reuses about 90% of `uffd.go`/`uffd_linux.go`/`uffd_engine.go`; net-new is the
page-fetch RPC and the source pause/drain lifecycle.

## Why this beats deeplethe/forkd

- Same-node: we match their cheap CoW fork (about 3 MiB, tens of ms) with an engine
  we already have, run per-tenant in one pod. Parity on the demo.
- Cross-node: a swarm that outgrows one node spills to others via post-copy. They
  are single-node only and defer multi-node; we already have the k8s platform and
  the UFFD plumbing. This is a capability they cannot easily grow into.
- Platform: multi-node scheduling, operator/CRDs/gitops, managed hosted service,
  per-org tenancy, billing/metering, deny-all egress. They are single-node alpha
  with manual netns setup and no egress policy.

The winning claim is not "faster than forkd" (parity same-node); it is "the only
one that gives you cheap live forks AND scales the swarm past one machine, managed,
isolated, on your own k8s."

## The k8s interface and observability (load-bearing decision)

Layer 1 decouples the sandbox abstraction from the pod: one pod hosts many
same-tenant VMs, so `kubectl get pods` no longer maps one pod to one sandbox. This
is a deliberate design choice, and it must not be a footnote. The rule:

- The Sandbox custom resource (`mitos.run/v1 Sandbox`), NOT the pod, is and always
  was the k8s-native handle for a sandbox. This is already the project's stated
  posture (CLAUDE.md operating principle 3: "Sandboxes are not pods; never imply
  pod-scoped mechanisms govern them"). Today's one-pod-per-VM makes the pod LOOK
  like the interface; it never was.
- The Sandbox CRD stays 1:1 with a VM. Fork 100 sandboxes and `kubectl get
  sandboxes` still shows 100 Sandbox objects and the SDK returns 100 handles; the
  pod is a shared runtime host BELOW the CRD, invisible to the user. For the user
  this is simpler, not weirder: fork returns a cheap sandbox and they never reason
  about pods, exactly the laptop-simple bar.
- Operator traceability is preserved by surfacing the mapping on the CRD:
  `Sandbox.status` gains `hostPod` (the session pod name) and `vmId` (the intra-pod
  Firecracker VM id), so `kubectl get sandbox X -o yaml` still traces a sandbox to
  its host process even though `kubectl get pods` does not. The session pod also
  carries a label listing (or a count of) the sandboxes it hosts for reverse
  lookup. Debugging entry point moves from pods to Sandboxes; the CRD carries what
  an operator needs.

What genuinely changes, stated honestly (each acceptable ONLY because a session
pod holds ONE tenant's VMs):

- Pod-scoped k8s primitives act per-session-pod, not per-sandbox: NetworkPolicy,
  `ResourceQuota count/pods`, and the pod cgroup `memory.max`. Per-sandbox network
  policy granularity is lost (all VMs in a pod share one netns + one egress
  filter); per-sandbox hard memory isolation is lost (a sibling can OOM its
  siblings, the user's own workload). Per-org NetworkPolicy and quota still hold at
  the pod/namespace level.
- Per-sandbox scheduling is gone: the sibling VMs live and die with the pod's node.
  Already true for forks (live fork is node-local), so no new constraint there.
- Billing must count per-VM inside a shared pod (the metering already reports
  per-VM `MemoryUnique`/`MemoryShared`, so the accounting seam exists; the labels
  that route usage to a sandbox move from the pod to the intra-pod VM id).

Precedent: deeplethe/forkd markets exactly this (one controller pod hosts N
children, "the scheduler runs once at pod creation regardless of fan-out") as a
feature. The accepted pattern is that the product's own API is the interface and
the pod is plumbing. Mitos's product API is the Sandbox CRD plus the gateway/SDK;
keeping the CRD 1:1 and the pod a shared host is consistent with that, not a break.

Non-goal: cross-TENANT VMs in one pod. A session pod is one org/one fork-tree. The
moment two tenants would share a pod, all of the above stops being acceptable, so
the pod-per-session boundary is a hard invariant, enforced at claim time.

## Two hard guarantees the multi-VM model must keep (resource accounting + resilience)

These are not optional. The one-pod-per-VM model gets them for free; the multi-VM
model must engineer them explicitly, and no milestone ships without them.

### Guarantee A: a fork never overcommits a node's memory

Today each husk pod requests its guest memory HONESTLY (huskpod.go: Requests[memory]
= configured, no overcommit, because Firecracker holds guest RAM resident), the
scheduler packs by sum-of-requests, and a capacity-admission gate pends a claim
with a NoCapacity condition when the node budget is full (capacity_admission
envtest). So a fork that does not fit PENDS, it never crushes the node. CPU is the
only deliberate overcommit lever (50m request floor, burst to the cap).

Why CPU may be overcommitted but memory may not: they are different kinds of
resource. Memory is NON-compressible. Firecracker holds guest RAM resident, so a
guest page physically exists on the node or it does not; overcommitting memory and
having guests touch their pages exhausts physical RAM and the OOM killer kills
processes, including LIVE sandboxes with a user's work, which is catastrophic and
unrecoverable. CPU is COMPRESSIBLE (time-shared): a vCPU not executing costs about
zero, and the agent workload is bursty and mostly idle, so a low request (50m)
packs many idle husks densely while active VMs burst to the cap and the Linux CFS
scheduler time-slices them under contention. Overcommitting CPU fails SOFT
(throttling, slower VMs, everything still completes); overcommitting memory fails
HARD (OOM, dead VMs). Tradeoff to be explicit about: CPU overcommit means a burst
of N forked agents all doing heavy compute at once contend and each runs slower, so
"N agents at once" is a density number, not N-cores-of-simultaneous-throughput; a
latency-SLA tier dials the CPU request up toward the cap (less overcommit, less
density) as a per-tier knob. This asymmetry is why guarantee A is about MEMORY: a
fork must never push a node past its physical memory, but CPU contention is
acceptable graceful degradation.

Multi-VM breaks the automatic version of this: N VMs share ONE pod whose k8s
memory request is fixed at creation, so adding a VM to a pod could overcommit
WITHIN the pod (a sibling OOMs its siblings). CoW makes the TYPICAL footprint tiny
(shared parent once + each child's dirty set; measured per-child CoW-read floor
0.0039 MiB), but the WORST case (every child dirties its whole guest) is N x the
guest size. The design must therefore, before admitting a fork into a pod:

- account for the added VM against a per-pod memory budget AND the node budget, and
- keep the node honest at worst case, via one of: reserve the pod at a bounded max
  VM count up front; grow the pod request with k8s in-place pod resize as VMs join;
  or admit-and-spill, placing the fork in a NEW session pod (possibly on another
  node, accepting it is then a template re-fork not a live fork) when the current
  pod's budget is exhausted.

The capacity-admission gate must extend to the per-pod dimension: a fork that would
push a pod (or node) past its memory budget PENDS or spills, never overcommits.
Metering already reports CoW-aware MemoryUnique/MemoryShared, which feeds the
sizing; the reservation must still be safe at worst case, not the CoW-typical case.

### Guarantee B: a swarm always survives node loss by reconstructing from durable state

Today Mitos has real multi-node resilience for independent sandboxes: node registry
+ capacity heartbeats, fast node-loss eviction tolerations (shorter than k8s's 300s
default), re-pend and re-create on a surviving node from a per-node snapshot digest
(husk_pernode_digest), GC that only sweeps healthy nodes. That is k8s-grade
failover and the multi-VM model must not regress it.

The honest physics: node-loss recovery is a COLD restore from a durable snapshot,
not live-memory preservation. Resident guest RAM cannot survive a node crash (same
as any VM or any k8s pod losing process memory). A live-fork swarm is node-local
(checkpoint + resident memory on one node) and the multi-VM model CONCENTRATES the
blast radius (a pod or node loss drops that whole session's swarm at once). So the
resilience contract is: the session's DURABLE state (its snapshots and disk) always
survives node loss and the swarm is reconstructable by re-forking from a snapshot on
a surviving node; the in-memory LIVE state of a swarm is not preserved across a
crash until cross-node post-copy (Layer 3) exists. The milestone must ensure a
session is always reconstructable from a durable snapshot, degrade a pod/node loss
to a re-fork (not lost user work), and surface the node-outage status the single-VM
path already does.

## Milestones (each its own PR/issue; correctness first, then density, then reach)

1. [#759] Parent-resume. Correctness; in flight (PR #763).
2. [#760] Fork child inherits pool network/egress/resources. Correctness.
3. NEW: benchmark the raw-forkd CoW density on the hosted node shape, to confirm
   the about 3 MiB/fork holds on production hardware and set the honest target
   (feeds #753).
4. NEW: multi-same-tenant-VMs-per-pod (Layer 1). The density win. Flagged,
   default off. Largest change; needs its own design review as a new execution
   mode. Includes the Sandbox.status host mapping (hostPod + vmId) and the
   session-pod reverse-lookup label, so the CRD stays the 1:1 k8s-native handle
   while the pod is a shared host (see "The k8s interface and observability").
5. NEW: cheap live fork (Layer 2): UFFD-served live-parent memory + UFFD_WP async
   resume. Prefer the no-vendoring route first.
6. NEW: cross-node post-copy fork (Layer 3): the memory-page server + network UFFD
   handler, same-rack v1.

## Risks and open questions

- Layer 1 is a new execution mode, not a tweak; partial-failure semantics (one VM
  dies, pod survives), per-VM billing within a shared pod, and the shared-memcg OOM
  blast radius all need design.
- Layer 2 route (b) may not hit the latency without vendoring Firecracker; route
  (a) reintroduces a vendored-FC maintenance burden. Decide with a spike.
- Cross-node cost is a working-set network stream; set expectations that off-node
  forks are latency-cheap, not bytes-cheap. Do not claim cross-node swarms are as
  cheap as same-node.
- Marketing: per the standing decision, do NOT change copy; engineer toward the
  numbers. But note internally that the about 3 MiB and about 27ms claims are real
  ONLY in the raw-forkd/CoW engine, and the hosted path must actually run that
  primitive before those claims are honest for hosted users.
