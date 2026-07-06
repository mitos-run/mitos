# Warm-husk live fork: sub-second live fork, resumed source, matched isolation

Status: design, not started. Owner: TBD. Supersedes nothing; builds on the
hosted live-fork route (#710, #735) and the fork-child scheduling fix (#749).

## Why

The hosted live fork is now correct but does not deliver the product promise.
Measured on production v1.24.1 (2026-07-06, single KVM node, python warm pool):

- A live fork returns Ready in about 4.7s (`fork_time_ms` 4682 on one sample),
  against a warm-pool claim of 6 to 8ms on-node and about 376ms client-observed.
- State inheritance is correct: a child sees the parent's post-boot disk writes
  AND kernel-memory variables (verified: `DISK inherited: True`,
  `MEM inherited: True`).
- Forking FREEZES the parent: the SDK sends `pause_source: true`, the source is
  paused for the checkpoint, and nothing resumes it. A post-fork exec against the
  source times out at 30s. This breaks the "fork the winner, keep going" loop that
  the product is built around.
- The child comes up networkless and uncapped: the fork-child activation omits the
  pool's network, egress, and resource config, so a deny-all child has no working
  NIC (harmless there) but a networked-tier child would get NO network instead of
  the parent's allowlisted access, and every child loses the pool's CPU burst cap.

The promise is fork that is live AND fast AND many AND isolated, all at once.
Today we get live + isolated but not fast, and not safely many (each fork freezes
the parent). This plan closes that gap.

## The core insight

The 4.7s is not the cost of checkpointing memory. It is the cost of creating a
COLD POD per child. `reconcileHuskFork` calls `ensureForkChildPod` which builds a
brand new husk pod (`buildForkChildPod`), which must schedule, pull/start its
container, then restore. The warm-pool path is fast precisely because the husk pod
already exists, pre-booted and dormant; the live-fork path throws that away.

The fix is to make a live fork restore its checkpoint into a PRE-WARMED DORMANT
HUSK, the same machinery warm-pool claims already use, instead of creating a cold
pod. Then the client sees roughly a warm claim plus the checkpoint write, not a
pod cold start.

## Current path (ground truth, origin/main v1.24.1)

`internal/controller/sandboxfork_controller.go` `reconcileHuskFork` (approx 425):

1. Resolve source husk pod; requeue until `PodIP` set.
2. Once (guarded by `Status.ForkSnapshotTaken`): controller calls
   `ForkSnapshotOnHusk` over the source pod's mTLS control port ->
   `internal/husk/stub.go` `Stub.ForkSnapshot` does `vm.Pause()` ->
   `vm.CreateSnapshot(mem, vmstate)` -> resume UNLESS `pause_source`. Written to
   the source pod's own `--forks-dir` hostPath `<dataDir>/forks/<fork-id>`.
3. Per replica: `ensureForkChildPod` -> `buildForkChildPod` (a NEW pod), pinned to
   the source node via nodeAffinity, mounts the fork snapshot dir read-only at
   `/var/lib/mitos/snapshot`, rootfs cloned from the source's live CoW disk.
4. When the child pod is Running+Ready: mint token, `ActivateHuskPod` restores the
   checkpoint, record the child Ready.

Three defects in that path, in priority order:

1. Step 3 creates a cold pod (the 4.7s). The child never touches the warm pool.
2. Step 2 leaves the source paused when `pause_source` is set, and nothing resumes
   it. The SDK always sends `pause_source: true` (`sdk/python/mitos/direct.py`
   `_fork_one`, and `aio.py`).
3. Step 4's `ActivateRequest` omits `Network`, `Egress`, `Allow`, `BlockNetwork`,
   `AllowCIDRs` (only `SnapshotDir` and `Token`), and `buildForkChildPod` builds
   from an EMPTY `PoolTemplateSpec{}` so the child also loses the pool's cpu cap.
   The warm-claim path passes all of these (`sandboxclaim_controller.go`).

## Target design

### 1. Fork children claim a warm dormant husk, not a cold pod

Replace the cold `ensureForkChildPod` in the fork path with a claim against a
pool of pre-warmed dormant husks, then restore the fork checkpoint into the claimed
husk via the existing `Activate` / `LoadSnapshotWithOverrides` path.

Open technical question to resolve first (spike): can a dormant husk that was
pre-booted and pre-snapshotted for pool template A load a DIFFERENT node-local
snapshot B (the source's live checkpoint) at claim time? Firecracker
`/snapshot/load` replaces guest memory and vCPU/device state wholesale, so this
should work when the machine config matches (same vCPU count and memory size, same
device layout). The husk-stub `Activate` already accepts an arbitrary `SnapshotDir`,
which is the seam. The constraint this imposes: the fork child's warm husk must be
sized to the SOURCE's machine config, not an arbitrary pool default. Options:

- (a) Reuse the source's own pool warm husks (same sizing by construction), drawing
  down `warm.min` and letting the pool refill. Simplest; couples fork capacity to
  pool warm capacity.
- (b) A dedicated per-node fork-warm buffer sized to the common sandbox shape.
  More predictable latency under fork bursts; more warm husks resident.

Recommend starting with (a) behind a feature flag, measure, then decide on (b).

Latency target after this change: client-observed p50 under 500ms, dominated by
WAN plus the checkpoint write, with on-node fork approaching
`checkpoint_time + warm_restore` (tens of ms) rather than seconds. The real split
(checkpoint vs pod-create vs restore) must be measured first (see issue: hosted
fork bench) so the target is grounded, not asserted.

### 2. Resume the source after the checkpoint

The checkpoint is a point-in-time copy; once `CreateSnapshot` has written mem and
vmstate and the rootfs clone is captured, the source can resume with no loss of
consistency. `ForkSnapshot` should pause, snapshot, capture the rootfs clone, then
resume.

Compatibility gate (do this analysis before writing the fix, it is milestone
[#759]'s first step): `pause_source=true` is today's wire contract on the fork
route, and its current meaning is "leave the source paused after the checkpoint".
We must NOT silently reinterpret that field in place if any consumer relies on
leave-paused. Enumerate every consumer: the hosted SDKs (`direct.py`, `aio.py`,
the TS/Go SDKs), the control-plane fork handler, the raw-forkd `ForkRunning` path
(`internal/fork/engine.go`), and the forksnapshot tests. Two outcomes:

- If NO consumer actually depends on leave-paused (the hosted path sends
  `pause_source=true` only to get a consistent checkpoint and every caller wants a
  usable source back), then leave-paused is a latent bug, not a contract, and
  `ForkSnapshot` resuming after the checkpoint is a fix, not a break. Document the
  audit that shows no dependency.
- If a consumer (e.g. a snapshot-and-hold or snapshot-and-terminate flow) genuinely
  needs the source to stay paused, do NOT overload `pause_source`. Add an explicit
  field (for example `resume_source`, defaulting to true, or a separate
  `leave_paused`) and migrate callers, keeping the old behavior reachable.

The default outward behavior after this milestone must be: a fork leaves the source
running. Which mechanism (reinterpret vs new field) is chosen by the audit above.

Verify the rootfs clone is captured atomically within the paused window; if the
clone currently happens outside the pause, move it inside so mem and disk are a
single consistent point in time before resume.

Fork-correctness: resuming the source is unchanged from today's non-`pause_source`
path, which already resumes and is covered by the fork-correctness suite. The child
still gets the fail-closed RNG reseed via `NotifyForked`.

### 3. Match the child's isolation and resources to a normal sandbox

Thread the source's resolved pool template into the fork-child activation so the
child gets the SAME `Network` (tap + baked-NIC remap), `Egress` deny/allow chain,
and resource caps a warm-claimed sandbox gets. The raw path already resolves this
via `resolvePoolTemplate`; the husk path must do the same and pass
`huskNotifyNetwork(template)` + `huskEgressConfig(template)` into the child
`ActivateRequest`, and build the child pod with the resolved template rather than
`PoolTemplateSpec{}`.

This is load-bearing beyond performance: without it a networked-tier fork child has
no network, and if a future change ever made the baked NIC routable, an omitted
egress chain would be an isolation gap. Add a test asserting the fork-child
`ActivateRequest.Network != nil` and the egress config is populated from the pool.

## Milestones (each a separate PR/issue, smallest first)

1. Parent-resume fix (correctness, ship first) [#759]. `ForkSnapshot` resumes the source
   after capturing the checkpoint; drop the leave-paused behavior. Test: fork a
   sandbox, assert a subsequent exec against the SOURCE succeeds. Highest priority:
   today forking silently freezes the parent.
2. Fork-child isolation + resources [#760]. Thread the resolved pool template (network,
   egress, cpu cap) into the fork child. Test: fork-child `ActivateRequest` carries
   network + egress; child cpu cap equals the pool.
3. Hosted fork bench [#753]. Measure `checkpoint_time_ms` vs restore vs
   pod-create through api.mitos.run so milestone 4's target is grounded and the
   marketing number can be reconciled honestly.
4. Warm-husk live fork (the big one) [#761]. Fork children claim a warm dormant husk and
   restore the checkpoint into it. Spike the cross-snapshot restore question first,
   then wire the claim path behind a flag, measure, and roll out.

## Non-goals

- Changing the marketing claim. Decision (2026-07-06): leave the 27ms claim as the
  north star and engineer toward it; do not rescope the copy. Milestone 3's bench
  informs whether that target is reachable for the live path.
- GPU fork, cross-node fork. Live fork stays on the source's node by construction.

## Risks

- Cross-snapshot restore into a dormant husk may not work for all machine configs;
  the spike in milestone 4 gates the rest.
- Reusing pool warm husks (option a) couples fork capacity to `warm.min`; a fork
  burst can starve normal claims. Measure before committing; option (b) is the
  hedge.
- Memory packing on a single KVM node bounds concurrent live forks well below the
  100 advertised KVM slots (each husk holds about 512Mi resident). Independent of
  this plan but relevant to the "many" promise; track separately.
