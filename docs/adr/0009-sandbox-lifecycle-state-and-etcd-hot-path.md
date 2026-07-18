# ADR 0009: the Sandbox CR stays the durable record; scale the reconciler before moving the claim into the gateway

Status: proposed (2026-07-18)
Issue: #15 (controller-path benchmarks; the sustained claims/sec and density
curve). Related: `internal/saas/controlplane` (the hosted create path,
`forward.go`, `readywatch.go`), `internal/controller/sandboxclaim_controller.go`
(`reconcileHuskClaim`), `internal/controller/huskpod.go` (`markHuskPodClaimed`,
`unmarkHuskPodClaimed`, `selectDormantHuskPod`), `docs/husk-pods.md`,
`docs/failure-gc.md` (status elision, TTL GC), `docs/threat-model.md`,
`BENCHMARKS.md` (#15 item 2, and the CI control-plane datapoint added with this
ADR). Motivating external critique: Modal, "Scaling to 1 million concurrent
sandboxes in seconds" (the argument that running one Kubernetes control-plane
transaction per sandbox does not scale).

## Context

The external critique is narrow and, on its own terms, correct: the Kubernetes
control plane is a poor per-sandbox transactional store at very high create
rates. The named bottlenecks are real (kube-scheduler is `O(n x p)` and
serialized by default, roughly 100 pods/sec in a live cluster; etcd write
throughput is single-node bound and not shardable within a keyspace; per-object
watch and reconcile add load per object). The question this ADR settles is how
much of that lands on Mitos, and what, if anything, to change.

What Mitos already does that sidesteps the worst of it:

- Sandboxes are not pods. Warm husk pods are pre-scheduled and pre-admitted, so
  the claim hot path is `selectDormantHuskPod` + `markHuskPodClaimed` (an
  optimistic-locked label patch) + mTLS activate, NOT a kube-scheduler decision
  (`docs/husk-pods.md`, `sandboxclaim_controller.go:782` reconcileHuskClaim). The
  kube-scheduler `O(n x p)` ceiling is off the claim path by construction.
- The exec / files / run_code data path never touches the API server: SDK ->
  podIP:sandboxPort -> vsock -> guest agent.
- Create LATENCY is already solved. The create response is delivered by a WATCH
  on the single Sandbox (`readywatch.go`), not a poll tick. The code records that
  the tick quantization it replaced was the dominant cost: p50 545ms client
  observed versus 6-40ms on-node activate (`forward.go` pollReady doc comment).
  So the remaining question is throughput, not latency.
- Reconcile churn is already bounded. `writeClaimStatusIfChanged` deep-compares
  before every status write so a re-reconciling claim does not churn etcd or
  re-trigger its own watch (`docs/failure-gc.md`), and finished objects are TTL'd
  out of etcd.

What remains coupled: every create, fork, and terminate is a Sandbox CR write
plus a watch plus a reconcile that performs a small number of status writes.
Under the elision, the steady state is on the order of two etcd writes per
sandbox (the create and the terminal Ready status). That is real per-sandbox
control-plane load, and it is the thing the external critique points at.

The honest gap: we have not measured whether that residual binds. etcd sustains
thousands of small writes per second; warm pools already removed the scheduler;
the watch already removed the latency. Whether two writes per sandbox plus a
watch is a wall at Mitos's target scale is an open, measurable question, and it
was unmeasured. Deciding to move the claim out of the control plane before
measuring would be optimizing a bottleneck we have not shown exists, while taking
on real risk (below). This ADR therefore ties the decision to a number.

The measurement is now wired: `BENCHMARKS.md` #15 item 2 and the `kind-e2e`
control-plane throughput step produce a real achieved-claims/sec datapoint on
every CI run (mock engine, single node: the etcd/apiserver/reconciler ceiling,
not hardware density), and `bench/sustained-claims-throughput.sh` traces the full
density curve on the reference node by sweeping the arrival rate.

## Decision drivers

1. Do not weaken the single-writer invariant. Claim correctness rests on exactly
   one writer of the `mitos.run/claim` label per pool, bounded today by leader
   election (`reconcileHuskClaim` doc comment: "Leader election (one active
   reconciler) is what bounds concurrent claiming"). The atomic claim commit
   `markHuskPodClaimed` is optimistic-locked and race-safe, but the RELEASE path
   `unmarkHuskPodClaimed` is a plain `MergeFrom` with NO optimistic lock, safe
   only because "it is the claim that stamped the label releasing it"
   (`huskpod.go`). A second concurrent claimer breaks that assumption.
2. Do not move the cross-tenant activation and mTLS surface into the gateway
   without strong, measured reason. Activation delivers secrets and the
   per-sandbox token over mTLS and is a security-sensitive path
   (`CLAUDE.md` names `internal/controller` claim/activation and future
   token/attenuation code for a named human reviewer).
3. Preserve the create contract: return `{id, endpoint, phase, token}` on create.
4. Prefer horizontal scaling that mirrors the external system's own answer
   (a fleet of stateless schedulers) without adopting its consistency tradeoffs
   where we do not have to.

## Options considered

### A. Keep the CR as the record; scale the reconciler horizontally (recommended lever if the ceiling binds)

Keep the Sandbox CR as both the claim and the durable record. Remove the SERIAL
reconciler ceiling by sharding Sandbox objects across controller replicas
(consistent-hash or label-partitioned work queues, one lease per shard) so
reconcile throughput scales with replicas, the way the external system scales
with stateless scheduling servers. etcd stays the store; the single-writer
invariant is preserved because each shard is the sole writer of its own objects.

- Pro: no security-surface change; no new trust path; the claim/activation stays
  in the leader-elected (now shard-owned) controller; the fix is horizontal and
  boring.
- Pro: directly attacks the measured bottleneck if it is reconcile serialization
  rather than raw etcd write cost.
- Con: does not reduce the ~2 etcd writes per sandbox. If the wall is etcd write
  throughput itself (not reconcile serialization), sharding the reconciler does
  not move it; that case needs etcd-side work (a dedicated events/leases store,
  or fewer writes per sandbox) or Option C.
- New invariant to prove: shard ownership must guarantee no two replicas own the
  same Sandbox concurrently, or the single-writer claim invariant breaks.

### B. Inline claim AND activate in the gateway; write the CR asynchronously

The gateway does `selectDormantHuskPod` + `markHuskPodClaimed` + mTLS activate
inline and returns the endpoint, writing the Sandbox CR asynchronously as a
record. Maximum decoupling: etcd is fully off the create critical path.

- Pro: the strongest form of "no data store on the critical path."
- Con: the gateway needs husk mTLS client credentials, a new secret distribution
  and a materially larger blast radius for the cross-tenant boundary.
- Con: two writers of the claim label. The commit is safe (optimistic lock), but
  `unmarkHuskPodClaimed` is not optimistic-locked, the token Secret write assumes
  single ownership, and Sandbox status writes are un-serialized; all three are
  documented single-writer assumptions that a concurrent claimer corrupts.
- Con: a gateway that claims a pod then dies before writing the CR leaks a
  claimed pod with no owning object; needs orphan-claim GC.
- This is the highest risk and the largest security-surface move.

### C. Reservation split: gateway claims inline, controller adopts and activates

The gateway does ONLY the race-safe `markHuskPodClaimed` inline and writes the CR
referencing the already-claimed pod; the reconciler ADOPTS the pre-claimed pod
and performs activation, token minting, and status. Activation, mTLS, token, and
status stay single-writer in the controller.

- Pro: removes warm-pod selection churn from the reconcile loop and lets create
  commit a reservation immediately, without giving the gateway mTLS or activation.
- Con: still writes the CR (as the record); create still waits on the reconciler
  to activate (via the watch), so it does not fully take etcd off the path.
- Con: still two writers of the claim label; requires an optimistic-locked
  release and orphan-claim GC (a claim label whose owning CR does not exist).
- Middle risk; a fallback if A is measured insufficient and the etcd write itself
  is not the wall.

## Decision

1. The Sandbox CR remains the durable lifecycle record. We do not adopt Option B
   now. Moving the claim and activation into the gateway trades a measured,
   bounded security posture for an UNMEASURED throughput gain and is not
   justified until the CR-per-create path is shown to be the binding constraint.
2. Measurement gates the next step. The control-plane ceiling is now produced in
   CI and sweepable on the reference node (#15 item 2). No lifecycle-decoupling
   code lands until that number shows the CR-per-create path is the binding
   constraint at a concrete target scale.
3. If it binds, Option A (shard the reconciler) is the first lever, because it
   scales throughput horizontally without weakening the single-writer invariant
   or moving the security boundary. Option C is the reserved fallback if A is
   insufficient AND the wall is not raw etcd write throughput; Option B is not
   pursued without a separate ADR and a named security review.
4. Any decoupling work ships behind a flag, default off, with the CR-synchronous
   create path remaining the default, and lands with its threat-model delta and a
   named human reviewer for the security-sensitive claim/activation path.

## Threat-model delta (applied when code lands, not now)

This ADR moves no code, so `docs/threat-model.md` is unchanged today. The deltas
below are recorded so the reviewer of any implementation PR knows exactly what
the chosen option moves, per the CLAUDE.md rule that the threat model updates in
the same PR as the surface change.

- Option A: sharding must guarantee exactly-one-owner per Sandbox (a per-shard
  lease). New row: a sharding bug that lets two replicas own one Sandbox breaks
  the single-writer claim invariant; the mitigation is lease-fenced shard
  ownership plus the existing `markHuskPodClaimed` optimistic lock as the
  backstop. No new cross-tenant surface.
- Options B and C: the gateway gains the ability to stamp `mitos.run/claim` on
  husk pods, so it must re-check pool ownership (`huskPodOwnedByPool`) and the
  namespace boundary exactly as `selectDormantHuskPod` does today; the release
  path must become optimistic-locked or claim-name-guarded; orphan-claim GC
  becomes required or warm capacity leaks. Option B additionally gives the
  gateway husk mTLS client credentials: a new secret to distribute and a larger
  blast radius for the cross-tenant boundary, which is why B needs its own ADR.

## Consequences

- The near-term deliverable for "does the Kubernetes bet scale" is the NUMBER,
  not a rewrite. #15 item 2 is now exercised on every CI run and sweepable on the
  reference node; that is what converts the external critique from an unrebutted
  blog argument into a measured curve for Mitos's actual path.
- The honest public position is unchanged and now measurable: warm pools remove
  the scheduler, the watch removes create latency, elision and TTL GC bound
  churn, and the residual per-sandbox etcd write is a known, bounded cost whose
  scaling ceiling we now publish rather than assert.
- Where Mitos actually beats the cold-start-and-schedule model is the fork axis
  (node-local live fork, CoW density), which is orthogonal to this ADR and not in
  scope here; this ADR only settles the control-plane substrate question.
- If measurement shows the ceiling binds, the path is pre-decided (A, then C),
  behind a flag, with a threat-model delta and a named reviewer.
