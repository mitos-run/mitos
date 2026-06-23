# Capability budgets and attenuated per-sandbox tokens

Status: design + verifiable core. The attenuation core (`internal/captoken`) and
the API/error shapes in this document are implemented and tested. The runtime
wiring (controller materializing budget-gated forks, depth-aggregate accounting,
`status.budgetSpend`) is the follow-up plan in the last section.

This document is the normative design for issue #25 and is the companion to
`docs/api/v2-spec.md` section 3 (capability budgets), the #24 runtime protocol
(`proto/sandbox/v1/sandbox.proto`), and ADR 0007
(`docs/adr/0007-api-v2-three-noun-consolidation.md`).

## 1. The budget model

Every sandbox carries a budget set by its creator: an orchestrator, a developer,
or a parent fork. A budget is five non-negative ceilings:

| Field | Meaning |
| --- | --- |
| `maxForks` | self-initiated forks, counted depth-aggregate across the fork subtree |
| `maxCheckpoints` | self-initiated checkpoints (live state to a workspace revision) |
| `maxCpuSeconds` | cumulative guest CPU-seconds across the sandbox and its subtree |
| `maxLifetimeExtension` | total lifetime a sandbox may add via `ExtendLifetime` |
| `maxEgressBytes` | total network egress across the sandbox and its subtree |

The budget gates the three budget-gated self-service runtime RPCs of §4 of the v2
spec: `Fork`, `Checkpoint`, and `ExtendLifetime` (and the read-only `Budget` RPC
reports remaining allowances). Those RPCs are defined in
`proto/sandbox/v1/sandbox.proto`: `Fork(ForkRequest) returns (Operation)`,
`Checkpoint(CheckpointRequest) returns (Revision)`,
`ExtendLifetime(ExtendRequest) returns (Lease)`, and
`Budget(BudgetRequest) returns (BudgetStatus)`. The proto `BudgetStatus` already
reports `fork`, `checkpoint`, and `lifetime_extension` as remaining/limit
`Allowance` pairs; this design adds `cpuSeconds` and `egressBytes` to the budget
model and to the v2 `Sandbox` shape (the proto `Allowance` set is extended in the
runtime-wiring follow-up so the two stay in step).

### Go types

The declarative shape is two Go types in `api/v1`:

- `Budget` (`maxForks`, `maxCheckpoints`, `maxCpuSeconds`, `maxLifetimeExtension`,
  `maxEgressBytes`); counts and CPU/egress are scalars, the two duration/quantity
  fields use `metav1.Duration` and `resource.Quantity` to match the spec's
  `1h` and `1Gi` notation.
- `BudgetSpend` (`forks`, `checkpoints`, `cpuSeconds`, `lifetimeExtension`,
  `egressBytes`): the consumed counterpart, surfaced on status.

The token-bearing copy of `Budget`/`BudgetSpend` lives in `internal/captoken`
(without the Kubernetes `metav1`/`resource` dependency) so the attenuation core
is a pure, fuzz-testable package. The two copies are intentional: the
`api/v1` types are the declarative wire shape; the `internal/captoken`
types are the signed, intersected runtime shape.

### Where the types land (ADR 0007)

Per ADR 0007 the budget is a field on the v1 `Sandbox` noun
(`spec.budget`/`status.budgetSpend`). The `Sandbox` kind is the served
API in `mitos.run/v1`; the former v1alpha1 kinds (`SandboxTemplate`, `SandboxPool`,
`SandboxClaim`, `SandboxFork`) have been consolidated: `SandboxFork` is now
`Sandbox` with `source.fromSandbox` + `replicas`, and `SandboxTemplate` is inlined
into `SandboxPool.spec.template`. The `Budget` and `BudgetSpend` types are embedded
on the `Sandbox` spec and status; the runtime enforcement wiring is the follow-up
described in section 6.

## 2. Attenuated tokens (macaroon-style)

There is exactly one runtime token per sandbox. A token is a bearer credential
that carries the sandbox's capability `Budget` and a set of `Scope`s (`exec`,
`files`, `fork`, `checkpoint`, `extend`, `network`), sealed with an HMAC-SHA256
caveat chain. The implementation is `internal/captoken`.

### The never-widen property (load-bearing)

A fork's token is STRICTLY NARROWER than its parent's: every budget field is
`min(parent_remaining, requested)` and the scope set is a subset of the parent's.
Attenuation can NEVER widen any dimension. This is the load-bearing correctness
property and it is enforced at two layers:

1. `Attenuate(parent, requested, requestedScopes)` computes the child as the
   element-wise budget intersection and the scope-set intersection, so a child it
   produces is narrower by construction.
2. `Verify` re-walks the embedded caveat chain. Each token carries the claims of
   EVERY link in its chain. `Verify` checks, at every link, both the HMAC
   integrity of the link tag (constant-time compare; no field was mutated after
   sealing; the right key signed it) AND the never-widen invariant between the
   embedded parent and child claims (the child budget fits within the parent and
   the child scopes are a subset). A forged wider child therefore fails even when
   the attacker re-seals the HMAC correctly, because the widening is rejected
   structurally between the embedded parent and child claims, not just by the
   signature.

The token chain is bound parent-first: `tag[0] = HMAC(rootKey, claims[0])` and
`tag[i] = HMAC(tag[i-1], claims[i])`, so a link cannot be re-parented onto a
different (wider) parent without producing a different tag. The HMAC primitive is
the standard library (`crypto/hmac` + `crypto/sha256`); no new crypto is
invented. The minting key is the same kind of secret the control plane already
manages; rotation is a key swap on the `Signer`.

### Budget minus spend

A self-initiated fork delegates the parent's REMAINING budget, not its original
budget. `Budget.Remaining(spend)` subtracts `BudgetSpend` element-wise, floored
at zero, and that remaining budget is the parent passed to `Attenuate`. So a
child can never exceed budget-minus-spend on any dimension, and depth-N
attenuation is monotonically non-increasing on every field (proven by the
property and fuzz tests).

### Test evidence

`internal/captoken/captoken_test.go` is the proof:

- `TestAttenuateNeverWidensBudget` and `TestAttenuateNeverWidensScopes`:
  table-driven never-widen assertions, including the request-wider-than-parent
  and zero-parent cases.
- `TestForgedWiderChildFailsVerification`: a hand-forged wider child, RE-SEALED
  with the real key, still fails `Verify`.
- `TestTamperedSignatureFailsVerification`, `TestWrongKeyFailsVerification`: HMAC
  integrity and key binding.
- `TestDepthNAttenuationMonotonicallyNarrows`: a depth-20 chain that always
  requests the widest budget and is clamped at every link; every link verifies
  and no field ever widens.
- `FuzzAttenuateNeverWidens`: the property/fuzz test over random parent budgets,
  scope masks, and requested budgets; the child is never wider on any dimension
  and always verifies.

## 3. Secret inheritance: reissue by default

`secretInheritance: reissue` is the default and remains so: each fork gets fresh
credentials, never a copy of the parent's. The attenuated token is itself a fresh
credential minted for the child (a new chain link, a distinct serialized value),
never the parent's token value. This is consistent with `docs/fork-correctness.md`
§3 and the existing `Sandbox.spec.secretInheritance` field (default `reissue`).
A fork carries a strictly-narrower token; it does not carry the parent's secrets
unless the source explicitly opts in to `secretInheritance: inherit`.

## 4. A self-initiated fork is a controller-materialized object

Mechanically, a self-initiated `Fork()` is NOT a side channel. The runtime RPC is
a request; the controller materializes a real `Sandbox` object owner-referenced
to the parent. RBAC, ResourceQuota, Events, OpenCost, and the audit log see the
child exactly as if an operator had created it: a normal object with an owner
reference, reconciled by the same controller path as any other sandbox. The agent
gets agency (it can fork itself for tree search, checkpoint before a risky step);
the audit ledger stays complete. This is the "exception-that-isn't" of the v2
spec: the imperative surface only ever talks to live sandboxes, and anything that
multiplies infrastructure still materializes as a declarative object through the
API server.

The owner reference is what makes depth-aggregate accounting tractable: the
controller walks the owner-reference subtree to sum `BudgetSpend` and clamp each
child's delegated budget to the parent's remaining.

## 5. Budget-exhaustion error

When a budget-gated RPC is refused because the dimension is spent, the runtime
returns the LLM-legible error `budget_exhausted` (HTTP 403, gRPC
`PermissionDenied`), defined in `internal/apierr` (`CodeBudgetExhausted`) and the
normative catalogue `docs/api/errors.md`. 403 (not 429) is deliberate: the
sandbox cannot retry its way to a wider creator-set budget. The remediation names
the orchestrator escalation path: the in-sandbox agent cannot widen its own
budget; it must request a larger budget from the orchestrator or operator that
created the sandbox (raise `spec.budget` on the parent `Sandbox`), or proceed
within the remaining budget reported by the `Budget` RPC. The error's structured
`context` names the exhausted `dimension` and the `remaining` allowance so the
reading model knows exactly which ceiling it hit.

## 6. Runtime-wiring follow-up (out of scope for this slice)

This slice is the verifiable core plus the API and error shapes. The following is
the multi-slice follow-up that spans the controller and the #24 Connect runtime:

1. Mint the per-sandbox root token at claim/Sandbox creation with the creator-set
   `spec.budget`, store it as the existing `<name>-sandbox-token` Secret value
   (replacing the opaque random token with a sealed `captoken`), and hand the
   forkd sandbox API the `Signer` so `requireBearer` verifies and reads scopes.
2. Wire the runtime `Fork`/`Checkpoint`/`ExtendLifetime` handlers to check the
   presented token's budget against the operation, return `budget_exhausted` on
   exhaustion, and on `Fork` call the controller to materialize the child
   `Sandbox` owner-referenced to the parent, attenuating the child token from the
   parent's REMAINING budget.
3. Controller-side depth-aggregate accounting: walk the owner-reference subtree to
   sum spend, clamp each delegated budget, and surface `status.budgetSpend`.
4. Extend the proto `BudgetStatus`/`Allowance` set with `cpu_seconds` and
   `egress_bytes` so the runtime `Budget` RPC reports all five dimensions.

Sequencing gate (per CLAUDE.md): the fork-correctness suite and failure/GC
semantics stay green; the threat model (`docs/threat-model.md`) gains a row for
the token-attenuation surface when the runtime wiring lands (the surface does not
move in this design-only slice).

## Cross-references

- `docs/api/v2-spec.md` section 3: capability budgets, the budget block, and the
  attenuation requirement.
- `proto/sandbox/v1/sandbox.proto`: the #24 runtime `Fork`/`Checkpoint`/
  `ExtendLifetime`/`Budget` RPCs and their `Budget`/`Allowance` messages.
- `docs/adr/0007-api-v2-three-noun-consolidation.md`: why the budget lands on the
  v2 `Sandbox` noun and the v1 kinds stay unchanged.
- `internal/captoken`: the attenuation core and its property/fuzz tests.
- `docs/api/errors.md`: the normative `budget_exhausted` catalogue entry.
- `docs/fork-correctness.md` §3: secret-inheritance reissue default.
