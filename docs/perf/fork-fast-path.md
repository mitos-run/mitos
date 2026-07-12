# Hosted fork latency: measurement and the synchronous fast-path question

Status: investigation note. Measured on prod v1.38 (co-location ON, husk
connection reuse ON), 2026-07-08. This documents where the hosted co-located
fork latency actually lives, corrects a misleading metric, and evaluates whether
a synchronous fork fast-path (bypassing the CR-create -> watch -> reconcile ->
poll cycle) is worth building.

## How a hosted fork flows today

1. SDK `DirectSandbox.fork(n)` issues ONE POST per child to
   `/v1/sandboxes/<src>/fork`. It does NOT poll. The request is held open (client
   deadline 130s). One round trip per child.
2. The control plane (`internal/saas/controlplane/fork.go`) stamps `startedAt`,
   resolves the org-owned source, `k.c.Create`s a `Sandbox` CR whose source is
   `fromSandbox`, then `pollReady` WATCHes that single object
   (`readywatch.go`, field selector on `metadata.name`) until the child is Ready,
   and returns the create-shaped body incl. `fork_time_ms`.
3. The controller watch fires, the fork reconciler
   (`sandboxfork_controller.go`) runs. On the co-located happy path it stamps
   `status.forkStartedAt`, takes ONE fork snapshot on the source husk pod
   (`fork_snapshot_rpc`), spawns the child as an additional VM inside the source
   pod (`spawn_vm_rpc`), writes status Ready. The control-plane watch observes
   Ready and the held-open POST returns.

So the client's wall clock is: `network RTT + CR create + [CR-create -> first
reconcile scheduling] + fork work + ready-watch delivery`.

## Measured, prod v1.38, co-located, 5 forks of a warm parent (WAN client)

Quota is 2 concurrent sandboxes on the test org, so forks were run one child at a
time (parent + one child), spaced. All were the co-located path (child VM spawned
in the source pod; no `sandbox-<id>-fork-0` pod), `reconcilePasses = 1`.

| fork      | client wall ms | server `fork_time_ms` | controller `totalMs` | fork_snapshot rpcMs | spawn_vm rpcMs |
|-----------|---------------:|----------------------:|---------------------:|--------------------:|---------------:|
| #1 (cold) |          795.7 |                   322 |                726.6 |                99.4 |          142.8 |
| #2        |          410.2 |                   266 |                552.9 |                77.6 |           93.3 |
| #3        |          437.5 |                   260 |                429.8 |                89.3 |           93.2 |
| #4        |          436.6 |                   248 |                791.8 |                70.6 |           92.9 |

- client wall p50 ~437 ms (warm 410-437; the first fork, 796 ms, is a cold
  TCP/TLS + first-connection outlier).
- server `fork_time_ms` (control-plane submit -> Ready) p50 ~260 ms (warm
  248-266; cold 322).

Per-stage controller instrumentation (ms), representative warm fork:

- `fork_snapshot_rpc` rpcMs 70-99, huskLatency 66-95 (create_snapshot 42-52,
  rootfs_freeze 16-46, freeze 6-14). rpcMs - huskLatency ~= 5 ms (conn reuse
  working).
- `spawn_vm_rpc` rpcMs 93-143, huskLatency 70-119 (egress_filter 25-27,
  guest_ready 18-47, handshake 16-27, rootfs_clone 16-18 [full copy], vmstate_restore 9-17).
  rpcMs - huskLatency ~= 20 ms.
- k8s orchestration (colocation_list, dial_tls x2, status writes x3,
  child_token_write, pool_get) sums ~50-59 ms.

## Finding 1: the co-located fork is ONE reconcile pass. RequeueAfter is not hit.

`reconcilePasses = 1` on every measured fork. The co-located path runs
`fork_snapshot` -> `spawn_vm` -> status Ready inside a single
`reconcileHuskFork` call and returns `ctrl.Result{}` (no requeue). The 1-2 s
`RequeueAfter` values in the reconciler are the NEW-POD fallback (wait for a
freshly scheduled child pod to become Running) and the source-not-Ready wait;
neither is on the co-located happy path. So there is NO multi-pass, 1-2 s
inter-pass requeue tax on a co-located fork.

## Finding 2: the controller `totalMs` OVERSTATES and is decorrelated from client latency

`totalMs` ("fork timing complete", `time.Since(forkStartedAt)`) ranges 430-792 ms
while the client-facing server number (`fork_time_ms`, submit -> Ready) is only
248-266 ms warm. Fork #4 is the proof: `totalMs` 791.8 ms but server submit ->
Ready 248 ms and client wall 437 ms. The LARGEST `totalMs` corresponds to the
SMALLEST client latency. `totalMs` therefore does not track what the client
waits for and must not be used as the client-latency proxy. The control-plane
watch returns when it observes the child Ready; the controller's `totalMs`
keeps measuring to the end of the reconcile pass, which continues past the point
the client was already served.

## Finding 3: the CR-create -> reconcile "scheduling gap" is small on the p50 client path, not ~200 ms

The instrumented server-side work (RPC ~165-242 ms + k8s orchestration
~50-59 ms = ~215-300 ms) already accounts for the control-plane submit -> Ready
of ~248-266 ms, leaving only ~20-35 ms of residual (CR-create round trip +
ready-watch delivery). The earlier "~200 ms reconcile-scheduling gap" was derived
by subtracting instrumented stages from the OVERSTATED `totalMs`, and by using a
noisy high client sample. It is an ARTIFACT of the misleading metric, not a
consistent p50 tax on the client path.

Note on resolution: the Sandbox CR YAML timestamps (`creationTimestamp`,
`status.forkStartedAt`, Ready `lastTransitionTime`, `checkpointTime`) all read the
SAME second (`metav1.Time` is second-resolution in YAML), so the sub-second
create -> forkStartedAt gap cannot be read off the CR. It is bounded small by the
arithmetic above.

## Verdict: consistent cost or tail jitter?

The client variance (410 vs 796 ms) is TAIL jitter, dominated by cold TCP/TLS +
first-connection setup, not a structural p50 floor. Warm forks are tight
(410-437 ms client, 248-266 ms server). The one place a real, load-dependent
scheduling gap CAN appear is workqueue contention: the sandbox reconciler runs
serially (`MaxConcurrentReconciles = 1`) across every Sandbox, SandboxFork, and
husk-pod event in the namespace, so under concurrent fork fan-out the
CR-create -> first-reconcile wait can spike. That is a THROUGHPUT / p99 concern,
not a p50 one.

## Synchronous fork fast-path: design

Idea: when the fork API request arrives, drive `fork_snapshot` -> `spawn_vm` ->
`activate` INLINE in the request handler and return when the child is ready,
persisting the Sandbox CR/status for record-keeping WITHOUT the client waiting on
the CR-create -> watch -> reconcile-schedule -> poll cycle.

Shape:
- The gateway handler resolves the source pod (as the reconciler does), calls the
  same `ForkSnapshotOnHusk` + `SpawnVMOnHusk` husk RPCs directly, mints the child
  token, and returns the ready child.
- The Sandbox CR is still created (before or right after the inline work) so the
  object exists for GC, ownership, crash recovery, and `getOwned` org-scoped
  lifecycle authz. Status is reconciled to Ready either inline or by the
  controller adopting the already-live child (idempotent, `AlreadyActive`).

Correctness constraints (must all still hold):
- The CR MUST exist for GC / owner-refs / crash recovery. The husk-fork finalizer
  (node-local snapshot cleanup) and the child pod owner-ref reaping depend on it.
- Leader election: today only the leader reconciler drives forks. An inline
  gateway path runs in EVERY gateway replica, so it needs its own concurrency
  control and must be safe against a concurrent reconcile of the same object
  (idempotency via the stable child name + token, `AlreadyActive` adoption -
  already implemented in `spawnForkChildInSourcePod`).
- The co-location budget (`coLocatedForkVMBudget`) AND the cross-fork reservation
  (`coLocatedVMsInPodByOtherForks`, the whole-pod ceiling minus VMs other forks
  placed) MUST still be enforced inline, or concurrent same-source forks
  over-admit VMs into one pod (intra-pod OOM). This read-modify-write currently
  relies on the serial reconciler; inline in N gateway replicas it needs a real
  lock or an admission gate.
- The one-snapshot-per-fork invariant (`ForkSnapshotTaken`, persisted before any
  child) must survive a crash between snapshot and spawn so the source is not
  re-paused / the fork point not split.
- Idempotency: a transparently retried POST (SDK auto-generates an
  Idempotency-Key) must not double-create a sibling.

## Estimated achievable drop

Modest, and NOT the headline. The sync fast-path removes the CR-create + watch
delivery + reconcile-schedule overhead, which the measurements bound at ~20-35 ms
server-side on the warm p50 path. It does NOT touch the two dominant costs:

1. client <-> api network RTT (~150-180 ms of the ~437 ms client wall) - only
   client-side connection reuse (~90 ms TCP+TLS) helps this.
2. husk RPC work (~215 ms: rootfs_clone full copy 16-46 ms, guest_ready 18-47 ms,
   create_snapshot 42-52 ms, egress_filter 25-27 ms, handshake, residual framing)
   - the fast-path runs the SAME RPCs, so this is unchanged.

Where the sync fast-path IS worth it: TAIL latency and throughput under
concurrent fork fan-out. It removes the serial `MaxConcurrentReconciles = 1`
workqueue from the fork hot path, so p99 under load (many simultaneous forks)
stops queuing behind unrelated Sandbox/pool reconciles. Treat it as a
p99 / throughput rearchitecture, not a p50 win.

## Ranked levers (from the measured breakdown)

1. Client-side connection reuse (~90 ms TCP+TLS) - biggest single client-wall win.
2. rootfs reflink instead of full copy (~16-46 ms) - node FS currently lacks reflink.
3. guest_ready (18-47 ms) + create_snapshot (42-52 ms) husk-side tuning.
4. Synchronous fast-path - ~20-35 ms p50, real p99 / throughput win under load.
5. Fix the misleading `totalMs` (measure to the Ready write, or log the
   client-correlated submit -> Ready) so future work targets the real number.

This note ships with lever 5's first half: the control plane now logs
`fork served` with `submitToCreateMs` and `submitToReadyMs`, the authoritative
client-correlated server-side split, independent of the controller `totalMs`.
