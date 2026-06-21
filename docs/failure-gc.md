# Failure and GC semantics

This document enumerates the failure and garbage-collection guarantees the
control plane provides today, the test that proves each one, and the time bound
within which it holds. It also states what remains open and points to the
tracking epic.

Two control loops cooperate:

- the SandboxClaim reconciler (`internal/controller/sandboxclaim_controller.go`),
  event-driven per claim, owns the finalizer reap and the lifetime/idle reap;
- the GarbageCollector (`internal/controller/gc.go`), a periodic Runnable that
  runs every `Interval` (default 30s), owns NodeLost, the VM orphan sweep, the
  volume orphan sweep, and TTL.

Tunables and their defaults (see `gc.go`):

- `Interval`: 30s. The period between GC passes; the bound on NodeLost and the
  orphan sweep.
- `OrphanGrace`: 60s. Minimum uptime before a backing-less VM is swept, so a
  just-forked VM whose claim status has not landed is never killed.
- `DefaultTTLSeconds`: 600s. TTL for a finished claim that does not set
  `spec.ttlSecondsAfterFinished`.
- finalizer terminate RPC timeout: 10s (`terminateOnNode` in `finalizer.go`).

## Guarantees

### Finalizer reap: a claim never disappears without its VM being reaped

Every claim acquires the `mitos.run/forkd-terminate` finalizer before it
acquires a VM. On delete the reconciler calls forkd `Terminate` on the claim's
node, then removes the finalizer. The RPC is bounded at 10s and tolerant: a node
that has left the registry, is unhealthy, cannot be dialed, or answers
`NotFound`, `Unavailable`, or `DeadlineExceeded` is treated as already
terminated, so a delete never wedges on an unreachable node. Any other error is
returned so a genuinely-reachable forkd that rejects the call is retried.

- Bound: the backing VM is reaped before the object is removed; the reap RPC is
  bounded at 10s.
- Proving tests: `TestClaimDeleteReapsBackingVM`,
  `TestClaimDeleteWithGoneNodeCompletes`,
  `TestClaimDeleteWithUnreachableForkdCompletes`.

### maxLifetime: a Ready claim is reaped at its wall-clock deadline

A Ready claim with `spec.timeout` set reaches the terminal `Terminated` phase
once `StartedAt + timeout` passes. The reaper terminates the VM, stamps
`FinishedAt`, and sets a `Terminated` condition with reason `MaxLifetimeExceeded`.
maxLifetime does not depend on a reachable forkd for the decision.

- Bound: terminal within a reconcile after the deadline.
- Proving test: `TestClaimMaxLifetimeReaped`.

### idleTimeout: an inactive Ready claim is reaped

A Ready claim with `spec.idleTimeout` set is reaped once it has been idle past
the timeout, measured from the later of `StartedAt` and last activity. Idle is
WORK-AWARE (issue #218): activity comes from forkd via the `ListSandboxes`
primitive, which reports each sandbox's last exec or file activity AND the
work-aware signals: the count of OPEN streams (a running background job) and the
paused flag. A sandbox with a live background process, or one that is paused, is
NOT idle, so an unattended job is never reaped mid-run. A live `set_timeout`
deadline takes authority over the idle clock: while it is set and in the future
the sandbox is not idle-reaped, and a past live deadline reaps with the
`TimeoutExpired` reason. A claim kept active is not reaped within the window; an
unreachable node defers the decision (requeue) rather than reaping blindly.
Reason on the `Terminated` condition is `IdleTimeout`. See `docs/lifecycle.md`
for the full lifecycle reference (timeouts, pause/resume, expiry).

- Bound: terminal within a reconcile after the idle deadline, given a reachable
  forkd.
- Proving tests: `TestClaimIdleTimeoutReaped`,
  `TestClaimIdleTimeoutNotReapedWhenActive`,
  `TestClaimIdleTimeoutNotReapedWithBackgroundJob`,
  `TestClaimSetTimeoutExtendsLiveTTL`, and the pure decision unit tests in
  `internal/controller/idle_decision_test.go`.

### Orphan sweep: a backing-less VM is reaped, with a live-claim-by-name net

Each pass, the GC lists sandboxes on every healthy node and terminates any whose
id is in neither the per-node desired-alive set (Ready claims and Ready fork
children, keyed by node and id) nor the node-independent liveID set, and whose
uptime exceeds `OrphanGrace`.

The liveID net is the safety valve. The controller uses `claim.Name` AS the
sandbox id (the claim reconciler forks with `claim.Name` and forkd echoes it
back, so `status.SandboxID == claim.Name` once Ready). So the liveID set is
every non-terminal claim by name UNION every non-terminal fork child by its
explicit `SandboxID`. A VM whose claim is wedged in `Restoring` or `Pending`
past the grace, and never wrote its status, is still recognized by name and left
alive. A VM becomes a sweep candidate only once its claim object is gone (or its
node is lost). This is a deliberate bound: a claim wedged in a non-terminal phase
keeps its VM alive by design.

When the sweep reaps a VM whose sandbox id still matches a present claim, that
claim is necessarily in a terminal phase (a non-terminal claim by name is in the
liveID net and never swept): the re-adopted-orphan case, where a claim reached
its terminal transition but its VM lingered (a terminate that crashed or was
missed, then re-adopted by a restarted forkd). The GC stamps a typed
`OrphanReaped` condition (`Ready=False`) on that still-present claim so an
operator or SDK can tell a GC reap apart from a graceful terminate. The stamp is
idempotent.

- Bound: a genuine orphan (no backing object) is reaped within one `Interval`
  once its uptime exceeds `OrphanGrace`.
- Proving tests: `TestGCSweepsOrphanVMs` (orphan past grace swept; fresh orphan
  and backed VM left alone), `TestGCLiveClaimByNameNotSwept` (live claim's VM by
  name not swept while the claim exists; swept after the claim is deleted),
  `TestGCStampsOrphanReapedConditionOnTerminalClaim` (a terminal claim whose
  lingering VM is swept carries the typed `OrphanReaped` condition).

### Volume orphan sweep: a backing-less volume backing is reclaimed

Each pass, after the VM sweep, the GC lists per-sandbox volume backing
directories on every healthy node (forkd `ListVolumes`, a disk scan of
`<dataDir>/sandboxes/<id>/volumes` so a backing whose sandbox is gone is still
reported) and reclaims (`ReclaimVolume`) any whose sandbox id is in neither the
desired-alive set nor the liveID set and whose age exceeds `OrphanGrace`. A
volume backing is keyed by the same sandbox id (the claim name) as the VM, so the
same desired and liveID sets and the same grace and live-object nets apply
unchanged: a backing for a non-terminal claim by name is left alone, a backing
younger than the grace is left alone, and only healthy nodes are visited.
`reclaimVolumeOnNode` is bounded and tolerant exactly like the VM
`terminateOnNode`. This closes the gap where a terminate that crashed or was
missed left the VM's backing files behind after the VM itself was reaped.

- Bound: a genuine volume orphan (no backing object) is reclaimed within one
  `Interval` once its age exceeds `OrphanGrace`.
- Proving test: `TestGCSweepsOrphanVolumes` (volume orphan past grace reclaimed;
  backed and fresh backings left alone).

### Controller-restart reconciliation: desired state is rebuilt from CRDs

The GC holds no in-memory desired state. Each pass rebuilds the desired-alive and
liveID sets purely from CRD state (claims and forks) and reconciles them against
forkd-reported actual VMs. After a controller restart the first pass therefore
sweeps any VM not accounted for and leaves every backed VM alone, with no
bootstrap window where state is lost.

- Bound: reconciled within one `Interval` of the restarted controller starting.
- Proving test: covered structurally by the orphan-sweep tests, which drive a
  fresh `GarbageCollector` with no prior state against live forkd VMs
  (`TestGCSweepsOrphanVMs`, `TestGCLiveClaimByNameNotSwept`).

### Node health: liveness, not just last-seen

A node is schedulable only while it is healthy. Health requires BOTH a recent
heartbeat (the 2-minute last-seen TTL on `LastHeartbeat`) AND a live forkd: a
node whose forkd liveness probe (the discovery `GetCapacity` call, every 15s)
fails `probeFailureThreshold` (3) times in a row is marked unhealthy and dropped
from `SelectNode`, even with a fresh heartbeat. This closes the gap where a pod
stays `Running` while forkd is hung or the host is dead: previously such a node
stayed healthy and schedulable for the full 2-minute TTL on stale capacity. The
threshold absorbs a transient single-probe blip (no flapping); at the 15s
interval, 3 failures is roughly 45s before the node leaves the schedulable set,
well inside the heartbeat TTL.

- Bound: roughly `probeFailureThreshold * discovery interval` (about 45s) before
  a hung forkd's node is dropped from scheduling.
- Proving tests: `TestNodeUnhealthyAfterProbeFailureThreshold`,
  `TestSyncPodsDropsNodeOnRepeatedProbeFailure`.

### NodeLost: a raw-forkd claim on a lost node reaches a terminal phase

In RAW-FORKD mode, a Ready claim whose node is no longer a healthy registered
node is transitioned to the terminal `Failed` phase with a `NodeLost` reason and
`FinishedAt` stamped. The node is gone, so there is nothing to terminate; the GC
only stamps state. The ephemeral VM died with the node and there is no recovery,
so failing the claim (and letting the TTL pass reap it) is correct. The orphan
sweep and NodeLost never fight: the sweep visits only healthy nodes, so a claim
on a lost node is never swept. A claim on a still-healthy node is untouched.

In HUSK mode, `markNodeLost` is a no-op: a Ready husk-backed claim recovers from
node loss by RE-PENDING onto a replacement dormant slot (owned by
`checkHuskPodLost` and the husk pod watch, which the warm pool self-heals). The
GC must not race that re-pend into a terminal `Failed`, so it skips the
node-lost-fail entirely in husk mode. The GC carries `EnableHuskPods` from the
controller run mode to make this decision.

- Bound: raw mode fails within one `Interval` of the node going unhealthy or
  leaving the registry; husk mode re-pends on the pod event (or the claim's own
  requeue).
- Husk hard-node-loss latency is cluster-dependent, not a mitos GC interval. Husk
  node-loss recovery fires immediately on a pod delete or `DeletionTimestamp`
  event. But a HARD host loss where the pod object lingers `Running` with no
  `DeletionTimestamp` is bounded by the cluster's own unreachable-pod eviction
  setting (the `node.kubernetes.io/unreachable` taint toleration, k8s default
  about 5 minutes), since no pod event fires until the cluster evicts the pod.
  Operators wanting faster husk node-loss recovery should tune the unreachable
  toleration or the pod-eviction timeout; mitos cannot shorten it.
- Proving tests: `TestGCMarksNodeLost`, `TestGCLeavesHealthyNodeClaim`,
  `TestGCInHuskModeDoesNotFailNodeLostClaim`.

### TTL hygiene: finished objects are deleted, including early-failed claims

A claim in a terminal phase (`Terminated` or `Failed`) whose `FinishedAt` is
older than its effective TTL (`spec.ttlSecondsAfterFinished`, else
`DefaultTTLSeconds`) is deleted, which triggers the finalizer reap. A claim with
no `FinishedAt` is skipped, and a recently-finished claim survives until its TTL.

Crucially, the reconciler's early-failure paths (volume preparation, secret
resolution, token minting, fork, token-secret write) stamp `FinishedAt` when
they set `Failed`, so an early-failed claim is TTL-eligible instead of leaking in
etcd forever.

- Bound: deleted within one `Interval` after `FinishedAt + TTL`.
- Proving tests: `TestGCTTLDeletesExpiredFinishedClaim`,
  `TestGCTTLKeepsRecentFinishedClaim`, `TestGCTTLsEarlyFailedClaim`.

## Known bounds and open items

By design, a VM is reaped only once its claim object is gone or its node is lost.
A claim wedged in a non-terminal phase keeps its VM alive (the liveID net). This
trades a possible leak of a wedged claim's VM for never killing a live VM whose
status simply has not landed; the wedged claim is itself observable and
deletable, at which point its VM is swept.

Shipped (was open, now in main):

- forkd-crash supervision of running VMs: a restarted forkd recognizes its own
  pre-crash Firecracker processes from an on-disk journal and either re-adopts
  the live ones (so the controller GC can reconcile them) or reaps the dead
  ones' leaked artifacts (jailer chroot, rootfs CoW clone, fork network, uid),
  with a procfs PID-recycle guard before any kill (issue #12, the crash-reap PR).
- saturation behavior: the node enforces a per-node MaxSandboxes ceiling (an
  atomic slot reservation, fail-closed), the scheduler avoids a node at its
  ceiling, and the controller pends a claim with a typed NoCapacity condition
  then fails it with CapacityExhausted after a bounded MaxPendingDuration; a
  forkd ResourceExhausted or Unavailable re-pends rather than hard-failing.

Shipped since (now in main, all from issue #163):

- volume orphan GC: see the Volume orphan sweep guarantee above. A per-sandbox
  volume backing whose claim object is gone is reclaimed past `OrphanGrace`,
  mirroring the VM orphan sweep with the same live-object safety net.
- typed condition on a GC-reaped re-adopted orphan: the `OrphanReaped` condition
  documented under the orphan sweep guarantee.
- snapshot rebuild elsewhere after a raw-forkd holder-node loss: a raw-forkd pool
  holds only the per-node template snapshot (no standing VMs). When a snapshot
  holder is lost, `readySnapshotCountOn` counts only healthy holders, so the
  deficit reconcile (`createSnapshotsOnNodes`) redistributes the snapshot onto a
  surviving node to restore the replica count, with no operator action. Proven by
  `TestSnapshotRebuildsOnHolderNodeLoss`.

The following remain OPEN and are tracked in epic #12:

- raw-forkd CLAIM auto-replacement after node loss: in the husk default the warm
  pool self-heals a lost node's dormant slots and the claim re-pends onto a
  surviving slot, but a raw-forkd claim on a dead node fails (NodeLost) with no
  automatic replacement, because raw mode has no standing dormant capacity to
  re-pend onto (its forks are ephemeral). This is acceptable for ephemeral
  sandboxes; the caller re-claims. It is a product decision, not a missing
  mechanism, and is held as the documented skip
  `TestRawForkdClaimAutoReplacementAfterNodeLossOpen` with its design (re-issue
  the fork on a surviving snapshot-holder, which the snapshot rebuild above
  guarantees exists).
- status-update rate-limiting and batching: the SandboxPool reconcile elides a
  no-op status write (`writePoolStatusIfChanged` / `poolStatusUnchanged`), and
  the SandboxClaim reconcile now does the same on its steady-state pend
  re-asserts (`writeClaimStatusIfChanged`, the no-node, snapshot-not-yet,
  NoCapacity, husk-raced, and activate-failed paths), so a stuck claim
  re-reconciling every 1-5s no longer churns etcd or re-triggers its own watch
  (proven by `TestWriteClaimStatusIfChangedElidesNoOp`). Still open: the
  SandboxFork reconciler, and true coalescing/rate-limiting of genuine
  transitions (today only exact no-op writes are elided, not a bounded update
  interval).
- chaos CI suite: test/cluster-e2e/chaos-e2e.sh runs on the multi-node
  self-hosted KVM cluster via the cluster-e2e workflow and exercises pod-loss
  recovery, warm-pool self-heal, cross-node failover (stage 5: cordon a claim's
  node, assert the claim recovers on another node, uncordon), AND component
  kill -9 under load (stage 6: SIGKILL the controller + forkd with
  --grace-period=0 --force while a 3-claim storm activates, assert every claim
  still converges, the pre-existing claim is undisturbed, the components recover,
  zero claims are permanently stuck, and zero orphan VMs survive once the storm
  claims are deleted). The controller-restart-under-storm invariant (a fresh GC
  reconciles a storm purely from CRDs with zero orphans and zero stuck claims) is
  additionally proven without KVM in `TestGCChaosStormNoOrphansNoStuckClaims`.
  Still KVM-gated and open: kill -9 of the GUEST agent process inside the VM and
  the real-forkd-with-VMs crash (both need a real cluster and KVM, unreachable
  from GitHub-hosted CI), and process-crash variants beyond SIGKILL.
