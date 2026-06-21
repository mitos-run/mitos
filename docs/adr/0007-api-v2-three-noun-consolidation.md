# ADR 0007: API v2 consolidates four kinds to three nouns (Pool, Sandbox, Workspace)

Status: accepted (2026-06-21)
Issue: #23 (API v2 consolidate CRDs to three nouns). Related: docs/api/v2-spec.md
section 5, docs/api/v2-migration.md (the field-by-field conversion notes),
docs/adr/0001-facade-and-naming.md (the deferred-rename coordination),
docs/adr/0002-workspace-not-csi.md (the Workspace model), docs/conditions.md
(the reason-code catalogue), ROADMAP.md (W2 API v2).

This ADR records a DESIGN decision the code does not yet implement. Per the ADR
framework rule (docs/adr/README.md) that an ADR recording an unbuilt decision
must say so: the four v1alpha1 kinds (`SandboxTemplate`, `SandboxPool`,
`SandboxClaim`, `SandboxFork`) remain in force and unchanged in `api/v1alpha1`
as of this writing. This ADR specifies the consolidation and its conversion so
the breaking migration is a well-scoped follow-up; it does not perform it.

## Context

The v2 spec (docs/api/v2-spec.md section 5) reshapes the declarative layer to
three nouns under the rule "Pools prepare, Sandboxes run, Workspaces persist."
Today `api/v1alpha1` serves four kinds:

- `SandboxTemplate`: the image, init scripts, resources, volumes, network, and
  at-rest encryption flag that define what a sandbox is built from.
- `SandboxPool`: the warm-pool of snapshots and dormant husk pods, referencing a
  template by `templateRef`.
- `SandboxClaim`: a request for one running sandbox from a pool, carrying env,
  secrets, workspace binding, lifetime, and terminate-with-outputs.
- `SandboxFork`: a request to fork N sibling sandboxes from a live source.

Three forces make the four-kind shape worth consolidating:

1. **Template and pool are never independently useful.** A `SandboxTemplate`
   exists only to be referenced by exactly one `SandboxPool` (`templateRef`); the
   pool pins the image and resources at snapshot-build time. The split mirrors a
   Deployment that could not embed its own PodSpec. The Deployment-embeds-PodSpec
   pattern (inline template, optional `templateRef` for reuse) is the idiomatic
   Kubernetes shape and removes a kind whose only consumer is its sibling.

2. **Fork and claim are the same engine concept.** A `SandboxClaim` fork-restores
   one VM from a pool snapshot; a `SandboxFork` fork-restores N VMs from a live
   source's snapshot. In the engine (internal/fork) these are one operation with
   different sources. Modeling them as two kinds forces the SDK and the operator
   to learn two nouns for one idea, and it splits the lineage story (claim vs
   fork) that the workspace revision DAG already unifies.

3. **The naming collision deferred by ADR 0001.** ADR 0001 implemented the
   `agents.x-k8s.io` upstream facade and DEFERRED the `mitos.run` noun rename to
   the API v2 migration, explicitly to avoid two breaking renames: one for the
   facade and one for the v2 consolidation. That deferral only pays off if the v2
   consolidation IS the single breaking rename. This ADR is the other half of
   that commitment: it fixes the target noun set so the deferred rename lands
   exactly once.

The four-kind shape is not wrong; it is one kind too many on each axis
(template/pool, claim/fork). v2 collapses each axis.

## Decision: consolidate to Pool, Sandbox, Workspace, with one breaking migration coordinated with ADR 0001

The v2 declarative layer is three nouns in `mitos.run`, all retaining the
`Sandbox`-prefixed names the cluster already serves where a v1 kind survives
(`SandboxPool`, `Sandbox`, `Workspace`); the consolidation removes
`SandboxTemplate` and `SandboxFork` as standalone kinds and folds their fields
into `SandboxPool` and `Sandbox` respectively. The Workspace (and its
`WorkspaceRevision` companion) is unchanged: ADR 0002 already shipped it in the
target shape.

### 1. SandboxTemplate inlines into SandboxPool, with an optional templateRef

The pool spec gains an inline `template` carrying every field of today's
`SandboxTemplateSpec` (image, init, command, env, resources, volumes, network,
encrypted). The standalone `SandboxTemplate` kind is removed. `templateRef`
survives as the optional reuse alternative (the Deployment-embeds-PodSpec
pattern): a pool sets EXACTLY ONE of `spec.template` (inline) or
`spec.templateRef` (a reference to a shared template-shaped object). Inline is
the common path; `templateRef` is for the rarer case of several pools sharing
one template definition.

The exact field mapping is in docs/api/v2-migration.md; in summary the v1
`SandboxTemplateSpec` becomes the v2 `SandboxPoolSpec.template` (a
`PoolTemplateSpec`), and the v1 `SandboxPoolSpec.templateRef` becomes the v2
`SandboxPoolSpec.templateRef`, now mutually exclusive with the inline template.

### 2. SandboxClaim and SandboxFork fold into one Sandbox kind

The v2 `Sandbox` is one running sandbox. Its source is a discriminated union of
exactly one of three origins, replacing the v1 `SandboxClaim.poolRef` and the v1
`SandboxFork.sourceRef`:

```
source:                          # exactly one of:
  poolRef:     { name }          # was SandboxClaim.poolRef: a fresh sandbox from a pool snapshot
  fromSandbox: { name }          # was SandboxFork.sourceRef: a fork of a live sandbox
  fromRevision:{ workspace, revision }   # NEW: lineage resume from a workspace revision
```

`replicas` (from `SandboxFork.replicas`, default 1) folds the fan-out into the
same kind: `replicas: 1` with `poolRef` is one sandbox (the old claim);
`replicas: N` with `fromSandbox` is N indexed sibling children (the old fork).
The remaining fields collapse as follows, detailed field-by-field in the
migration notes:

- `resume: memory | filesystem` selects whether a `fromRevision`/`fromSandbox`
  source restores warm VM memory or only the filesystem. A cross-principal
  handoff forces `filesystem` (the memory-snapshot principal binding from ADR
  0002 and docs/fork-correctness.md).
- `budget` carries the v2 capability budget (maxForks, maxCheckpoints,
  maxCpuSeconds, maxLifetimeExtension, maxEgressBytes) for runtime self-service
  (v2-spec section 3). This is NEW surface; it has no v1 field and defaults from
  the pool's `defaultBudget`. The runtime enforcement is issue #25, sequenced
  after this consolidation.
- `lifetime.ttl` / `lifetime.idleTimeout` map from `SandboxClaim.timeout` /
  `SandboxClaim.idleTimeout`.
- `lifetime.onTerminate.outputs` maps from `SandboxClaim.outputs` (the
  `OutputSpec` shape: path, diff, git, already aligned to the v2 spec), and
  `lifetime.onTerminate.snapshot` (retain-last-N) generalizes
  `SandboxClaim.checkpointOnTerminate`.
- `secretInheritance: reissue | inherit` (default reissue) replaces the v1
  `SandboxFork.allowSecretInheritance` boolean. Reissue (the default) gives each
  fork fresh credentials; `inherit` requires source opt-in. This keeps the
  fork-correctness secrets policy (docs/fork-correctness.md section 3) but states
  it as the safer default rather than an opt-out.
- `workspaceRef`, `env`, `secrets`, `serviceAccount`, `network.extraAllow`, and
  `volumeOverrides` carry across from `SandboxClaim` unchanged in meaning.

`status` consolidates `SandboxClaimStatus` and `SandboxForkStatus`: a single
`Sandbox` reports `phase`, `endpoint`, `pod` (the husk pod name), `revision`
(produced on terminate), `budgetSpend`, `startupLatencyMs`, and `conditions`. A
`replicas > 1` Sandbox additionally reports per-child status (the old
`SandboxForkStatus.forks` list), so the fan-out remains observable under one
kind.

### 3. Status conventions target (mostly already in force)

The v2 status conventions are the ones docs/conditions.md and the existing
reconcilers already implement; this ADR records them as the target so the
migration does not regress them:

- Typed conditions with `observedGeneration` matching `generation`, a `Ready`
  condition per kind, and the docs/conditions.md reason-code catalogue as the
  normative contract. The catalogue's existing `SandboxClaim`, `SandboxPool`,
  and `SandboxFork` reasons map onto the consolidated kinds (the `SandboxFork`
  reasons `SecretInheritanceDenied`, `ExplicitOptIn`, `Forked`/`ForksCreated`
  become `Sandbox` reasons gated on `source.fromSandbox` with `replicas > 1`).
- Owner references for GC: a self-forked Sandbox owner-refs its parent (the
  budgeted-self-service ledger, v2-spec section 3); revisions owner-ref their
  workspace (ADR 0002).
- Every data-plane action mirrored as a Kubernetes Event.
- Finished sandboxes TTL'd from etcd: the v1 `SandboxClaim`
  `ttlSecondsAfterFinished` / `FinishedAt` GC carries to the v2 `Sandbox`
  unchanged.

These are conventions to PRESERVE, not new work in the consolidation slice.

## Migration path from v1alpha1

The consolidation is a breaking change to the `mitos.run` API surface: a kind is
removed (`SandboxTemplate`, `SandboxFork`), a kind gains required oneof shape
(`Sandbox.source`), and field paths move. v1alpha1 MAY break, but never
silently: docs/api/v2-migration.md is the field-by-field conversion record for
every breaking change, so an operator with v1 manifests has an exact mapping to
v2.

The path is the standard Kubernetes multi-version CRD migration:

1. Serve both versions during the transition. `Sandbox` and `SandboxPool` carry
   both `v1alpha1` and `v2` (or `v1alpha2`) in the CRD `versions` list, with a
   conversion webhook translating between them per the migration table. The
   removed kinds (`SandboxTemplate`, `SandboxFork`) convert into the surviving
   kinds: a stored `SandboxTemplate` converts to a `SandboxPool` with an inline
   template (or is referenced via `templateRef`); a stored `SandboxFork`
   converts to a `Sandbox` with `source.fromSandbox` + `replicas`.
2. Storage migration walks existing objects through the webhook so etcd holds the
   new shape, then the old version is marked `served: false` and finally removed.
3. The SDK, facade (ADR 0001), and controller cut over to the consolidated kinds
   in the same migration, since this is the SINGLE breaking rename the deferral
   bought.

The conversion is mechanical and total: every v1 field has a v2 destination (the
migration table), and every removed kind has a surviving host. No v1 information
is lost; only NEW v2 fields (`budget`, `resume`, `fromRevision`) have no v1
source and take documented defaults.

## Coordination with ADR 0001: exactly one breaking rename before 1.0

ADR 0001 Decision 2 deferred the `mitos.run` noun rename to "the API v2
migration ... with conversion webhooks / a documented upgrade path, before 1.0,"
precisely to avoid two breaking renames. This ADR is that migration's noun-set
decision. The contract between the two ADRs:

- ADR 0001 did NOT rename the v1 kinds for the facade; it group-qualified them in
  docs and waited. This ADR honors that by making the v2 consolidation the one
  place the noun set changes.
- The facade keeps mapping the upstream `agents.x-k8s.io Sandbox` onto our run
  path; after this migration it targets the consolidated `mitos.run Sandbox`
  (with `source.poolRef`) instead of a `SandboxClaim`. That re-target rides the
  same conversion, so the facade does not incur a separate breaking change.
- There is therefore exactly ONE breaking rename of the `mitos.run` surface
  before 1.0: this consolidation. ADR 0001's deferral is discharged here.

## Why not keep four kinds and add v2 as a thin alias

Keeping `SandboxTemplate` and `SandboxFork` and adding the consolidated shape
only as SDK sugar over four kinds was considered. Rejected: it leaves the
operator surface at four nouns, leaves two nouns for one engine concept
(claim/fork), and defeats ADR 0001's premise that v2 is where the noun set is
reshaped. The cognitive collision ADR 0001 tolerates only because v2 fixes it
would become permanent. The consolidation has to reach the CRDs, not just the
SDK.

## Why not do the consolidation now in this slice

This slice is deliberately the design foundation only, not the breaking
migration, for the same boring-failure-behavior reason ADR 0001 deferred the
rename: a half-done breaking migration destabilizes the controller, SDK, facade,
and the full test suite at once. Ripping out two kinds before the conversion
webhook, the storage migration, and the cutover are ready would leave the system
in a state with no boring failure behavior. The ADR plus the migration notes
fully specify the target and its conversion so the actual migration is a
well-scoped, testable follow-up (issue #23 continuation), gated like every
breaking change on the conversion path being green.

## Consequences

- The operator and SDK learn three nouns, not four; "Pools prepare, Sandboxes
  run, Workspaces persist" is the whole declarative vocabulary.
- Fork and claim become one `Sandbox` kind, so lineage (pool start, live fork,
  revision resume) is one `source` union and the workspace revision DAG is the
  single lineage story.
- The migration is breaking but total and documented: docs/api/v2-migration.md is
  the conversion contract; no v1 field is dropped silently.
- ADR 0001's deferred rename is discharged by this single migration; the facade
  re-targets the consolidated `Sandbox` on the same conversion.
- The new v2-only surface (`budget`, `resume`, `fromRevision`, the consolidated
  status) has documented defaults so a v1-to-v2 conversion never requires a field
  the operator must invent.
- Status conventions (typed conditions, observedGeneration, the reason
  catalogue, owner refs, Events, finished TTL) are preserved, not redesigned; the
  catalogue's `SandboxFork` reasons re-home onto `Sandbox`.

## Status of the slice

The DESIGN slice (this ADR and docs/api/v2-migration.md) landed first. The first
IMPLEMENTATION slice has now landed too, additive and non-destabilizing:

PROVEN now:

- The `v1alpha2` Go types for the consolidated `SandboxPool` (inline `template`
  plus optional `templateRef`, `snapshots`, `warm`, `placement`) and `Sandbox`
  (`source` oneof poolRef/fromSandbox/fromRevision plus `replicas`, folding the
  v1alpha1 SandboxClaim and SandboxFork into one kind) exist in `api/v1alpha2`.
- v1alpha1 `SandboxPool` is the conversion Hub (storage version); v1alpha2
  `SandboxPool` implements `conversion.Convertible` with PURE ConvertTo/
  ConvertFrom per the migration table, covered by round-trip unit tests and a
  conversion-webhook envtest that serves both versions through the API server.
- An opt-in consolidated `Sandbox` reconciler maps a Sandbox onto the EXISTING
  SandboxClaim/SandboxFork engine (additive), with a conformance envtest proving
  a Sandbox{source.poolRef} reaches Ready (claim equivalent) and a
  Sandbox{source.fromSandbox, replicas:N} produces N children (fork equivalent)
  on the mock engine.
- The four v1alpha1 kinds remain in force and unchanged; their CRDs do not drift
  (the sandboxpools CRD gains an additive v1alpha2 version; a new sandboxes CRD
  is added). Both surfaces serve.

OPEN (the breaking migration follow-up, issue #23 continuation): the
storage-version flip to v1alpha2; the removal of `SandboxTemplate` and
`SandboxFork`; the cross-kind storage migration (SandboxClaim/SandboxFork ->
Sandbox); the facade re-target; the SDK cutover; and the conditions catalogue
re-homing. Each rides the conversion-path-green gate before any production tenant
sees it.
