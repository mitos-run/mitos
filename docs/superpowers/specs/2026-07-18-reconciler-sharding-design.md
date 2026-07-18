# Reconciler sharding (Option A of ADR 0009): design spec

Status: ready to execute, gated on measurement (do not implement until the #15
item 2 curve shows a single reconciler is the binding control-plane constraint).
Decision record: `docs/adr/0009-sandbox-lifecycle-state-and-etcd-hot-path.md`.
Issue: #15. Grounding code: `internal/controller/sandboxclaim_controller.go`
(`reconcileHuskClaim`), `internal/controller/huskpod.go` (`markHuskPodClaimed`,
`unmarkHuskPodClaimed`, `selectDormantHuskPod`), `cmd/controller/main.go`
(manager + leader election wiring), `internal/controller/node_registry.go`.

## Problem

The `SandboxReconciler` runs as one leader-elected reconcile loop. Within the
active leader, controller-runtime drains a work queue with
`MaxConcurrentReconciles` workers, so the control-plane claim throughput ceiling
is one leader's aggregate reconcile rate. Leader election gives us HA (one active
leader, standbys idle), not horizontal throughput: the standbys do no work. When
the #15 item 2 curve shows achieved claims/sec plateauing below the arrival rate
with the leader CPU-bound and its work queue growing, that single-leader ceiling
is the wall, and this spec is how we lift it.

The hard constraint: the claim correctness invariant. Exactly one actor may
successfully claim a given warm husk pod. Today that is bounded by "one active
reconciler" (leader election). Any horizontal scheme must not let two reconcilers
corrupt a claim.

## Key insight: sharding is load distribution, not correctness

Correctness does NOT have to rest on perfect single-ownership of a Sandbox. The
pod-claim commit is already the true arbiter:

- `markHuskPodClaimed` (`huskpod.go`) patches the `mitos.run/claim` label under
  `client.MergeFromWithOptimisticLock`. Two actors that select the same dormant
  pod both attempt the patch; the API server lets exactly one win and the other
  gets 409, which the caller handles by pending + requeue (`HuskPodRaced`). This
  holds no matter how many reconcilers race.

So if the RELEASE path and the token/status writes are hardened against a second
writer, then sharding becomes a pure LOAD-DISTRIBUTION optimization: a brief
two-owner window (for example during a shard-count resize) is merely wasted work
(a 409 race), never corruption. This is what makes Option A cheap and safe, and
it is why we do NOT need a perfect exactly-one-owner lease as a correctness
mechanism. The single hardening prerequisite:

- Harden `unmarkHuskPodClaimed`. Today it is a plain `client.MergeFrom` with NO
  optimistic lock, justified by "it is the claim that stamped the label releasing
  it." With two potential claimers that justification fails: a stale owner could
  blindly clear a `mitos.run/claim` label a different claim just stamped. Fix:
  make the release a claim-name-guarded, optimistic-locked patch. It releases the
  label ONLY if the label still names THIS claim, else it is a no-op. This is a
  small, self-contained correctness improvement worth landing first and on its own
  merits, independent of sharding.

## Design

Three landable steps, in order. Steps 1 and 2 are safe to land before the gate
opens; step 3 (turning sharding ON) is what waits on the measurement.

### Step 1: harden the claim release (correctness, land anytime)

Change `unmarkHuskPodClaimed` to a claim-name-guarded optimistic-locked patch as
above. Add a unit/envtest that a release whose pod label names a DIFFERENT claim
is a no-op (does not un-claim the other claim's pod).

### Step 2: hash-in-predicate shard filtering (default single shard = today)

Each reconciler owns a set of hash ranges over `shard(namespace/name)`. It
reconciles object O only when `shard(O)` is in its owned set; otherwise the
predicate drops the event before it enters the work queue. No object is mutated
(no shard label, no webhook), so there is nothing to rewrite on a resize and no
extra etcd write per sandbox.

- `shard(key) = fnv32a(namespace + "/" + name) % shardCount`.
- Membership is STATIC in v1: `--shard-count N` (default 1) and `--shard-index i`
  (default 0), where `i` comes from the pod's StatefulSet ordinal via the downward
  API. `shardCount = 1` reproduces today's behavior exactly (one owner of
  everything), so the flag defaults to a no-op.
- Wire the filter as a `predicate.Predicate` on the `SandboxReconciler`'s
  `For(&v1.Sandbox{})` watch AND as a cache `ByObject` field/label restriction if
  cheap; the predicate is authoritative. Fork children and the pool/workspace
  watches shard by the OWNING Sandbox's key so a Sandbox and its forks land on the
  same shard (locality: a live fork is node-pinned to its source anyway).
- Leader election changes shape: with a StatefulSet of N controllers, each pod is
  the sole active reconciler FOR ITS SHARD. Either run per-shard leader election
  (a lease per shard index) so each shard still has HA standbys, or run the
  controllers as a StatefulSet where each ordinal is its own singleton. v1 uses a
  per-shard lease keyed by shard index, so HA is preserved within a shard.

### Step 3: turn it on (gated)

Flip `shard-count > 1` and scale the controller StatefulSet only when the
measurement (below) says the single-leader reconcile rate is the binding
constraint. Because sharding is load distribution and the lock is the arbiter, a
resize from N to N+1 (which remaps object->shard) needs no stop-the-world: during
the brief window where old and new owners disagree on a few objects, the
optimistic-locked claim + guarded release make the overlap harmless (wasted 409
races, no corruption). A rolling resize with the standard lease TTLs is
sufficient.

## Test plan (envtest, all offline)

- Release guard (step 1): a pod whose `mitos.run/claim` names claim B is not
  released by claim A's `unmarkHuskPodClaimed`; claim A's own release is a no-op
  once the label no longer names A.
- Shard partition (step 2): two `SandboxReconciler`s over one envtest apiserver,
  `shard-count=2`, indices 0 and 1. Assert each reconciles only keys whose hash
  matches its index (observe reconcile invocations), every Sandbox still reaches
  Ready, and no warm pod is double-claimed.
- Pathological two-owner (correctness backstop): two reconcilers BOTH index 0
  (simulating a resize overlap) claiming from one warm pool. Assert no pod carries
  two claims (the optimistic lock holds), no warm capacity leaks (guarded
  release), and every claim resolves to Ready or a clean pend, never corruption.
- Throughput (uses the #15 harness): run `bench/claim --mode sustained` against a
  controller scaled to N shards and confirm aggregate achieved claims/sec scales
  with N until another resource (etcd write rate, forkd) becomes the wall.

## The measurement signal that opens the gate

Flip sharding on only when the #15 item 2 data shows ALL of: achieved claims/sec
plateaus below the arrival rate; the active leader's reconcile work-queue depth
grows without draining; and the leader pod is CPU-bound (not blocked on etcd or
forkd). If instead the wall is raw etcd write throughput (leader CPU idle, etcd
fsync latency climbing), sharding the reconciler will NOT help and the decision
returns to ADR 0009 Option C or etcd-side work; record that outcome as a new ADR.

## Scope boundaries (YAGNI)

- No dynamic auto-rebalancing in v1: shard-count is a deliberate operator resize.
- No mutating webhook and no per-object shard label: hash-in-predicate only, so
  there is zero added per-sandbox etcd write.
- Options B and C of ADR 0009 (moving the claim/activation into the gateway) are
  out of scope; they require their own ADR and a named security review.
- Step 1 (release hardening) may land independently; it is a correctness fix, not
  a scaling change, and does not need the gate.

## Threat-model delta (applies when step 2/3 land)

Sharding introduces no new cross-tenant surface: no object is mutated, the
namespace boundary and `huskPodOwnedByPool` provenance gate are unchanged, and the
claim/activation stays in the controller. The one delta to record in
`docs/threat-model.md` when step 1 lands: the claim release is now
claim-name-guarded and optimistic-locked, closing the "stale actor blindly
un-claims a live pod" hazard that the single-writer assumption previously covered.
