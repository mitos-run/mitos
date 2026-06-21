# API v1alpha1 to v2: conversion notes

Status: design record for issue #23. Related: docs/adr/0007-api-v2-three-noun-consolidation.md,
docs/api/v2-spec.md section 5, docs/adr/0001-facade-and-naming.md,
docs/adr/0002-workspace-not-csi.md, docs/conditions.md.

This is the conversion contract for the API v2 three-noun consolidation. ADR 0007
records the DECISION; this document records the field-by-field MAPPING so the
v1alpha1 surface MAY break but NEVER silently. Every breaking change has a row
here. Nothing in code changes yet: the four v1alpha1 kinds
(`SandboxTemplate`, `SandboxPool`, `SandboxClaim`, `SandboxFork`) remain in
force and unchanged until the migration follow-up lands the conversion webhook
and storage migration.

The v2 (`v1alpha2`) Go types named below (`PoolTemplateSpec`, `SandboxSource`,
`SandboxLifetime`, etc.) are TARGET shapes; they do not exist in `api/v1alpha1`
today and are not added in this slice.

## Summary of breaking changes

| Change | v1alpha1 | v2 | Why |
| --- | --- | --- | --- |
| Kind removed | `SandboxTemplate` | inlined into `SandboxPool.spec.template`; `templateRef` survives as optional reuse | template is never independently useful; Deployment-embeds-PodSpec pattern |
| Kind removed | `SandboxFork` | folded into `Sandbox` via `source.fromSandbox` + `replicas` | fork and claim are one engine concept |
| Kind renamed | `SandboxClaim` | `Sandbox` | a claim IS the running sandbox; one noun |
| Required oneof added | `SandboxClaim.poolRef` (single source) | `Sandbox.source` oneof `{poolRef, fromSandbox, fromRevision}` | unify pool-start, live-fork, revision-resume |
| Field renamed | `SandboxFork.allowSecretInheritance: bool` | `Sandbox.secretInheritance: reissue\|inherit` | safer default stated explicitly (reissue) |
| New v2-only surface | (none) | `Sandbox.budget`, `Sandbox.resume`, `source.fromRevision` | capability budgets (#25), lineage resume; documented defaults |
| Kind unchanged | `Workspace`, `WorkspaceRevision` | unchanged | ADR 0002 already shipped the target shape |

A blank v2 cell means the field has no v2 counterpart and is dropped by the
conversion (each such row says why). A blank v1 cell means the v2 field is new
and takes the listed default on conversion.

## SandboxTemplate -> SandboxPool.spec.template (+ optional templateRef)

The standalone `SandboxTemplate` kind is removed. Its `SandboxTemplateSpec`
becomes the inline `SandboxPoolSpec.template` (a `PoolTemplateSpec`). A pool sets
EXACTLY ONE of inline `template` or `templateRef`. A stored `SandboxTemplate`
converts to either an inline pool template or a referenced template-shaped object
per the operator's choice during storage migration; the default conversion
inlines it into the single pool that referenced it.

| v1alpha1 `SandboxTemplateSpec` | v2 `SandboxPoolSpec.template` (`PoolTemplateSpec`) | Notes |
| --- | --- | --- |
| `image` | `template.image` | unchanged |
| `init` | `template.init` | unchanged; pool-build only, never per-sandbox |
| `command` | `template.command` | unchanged |
| `env` | `template.env` | unchanged (`[]corev1.EnvVar`) |
| `resources` (`SandboxResources`: cpu, memory) | `template.resources` | unchanged; v2 spec adds `balloon: true` as a resources field |
| `volumes` (`[]SandboxVolume`) | `template.volumes` | unchanged shape (name, size, source, readOnly, mountPath, forkPolicy, snapshotClass, storageClass) |
| `networkPolicy` (`*NetworkPolicy`) | `template.network` | unchanged shape (egress, allow) |
| `encrypted` | `template.encrypted` | unchanged; at-rest snapshot encryption flag |
| (none) | `template.defaultBudget` | NEW: pool default capability budget inherited by sandboxes; default empty (no budget) |

## SandboxPool (kept) field changes

`SandboxPool` survives. The only spec change is the template axis: `templateRef`
becomes one of a oneof with the new inline `template`.

| v1alpha1 `SandboxPoolSpec` | v2 `SandboxPoolSpec` | Notes |
| --- | --- | --- |
| `templateRef` (required) | `templateRef` (optional, oneof with `template`) | now mutually exclusive with inline `template`; exactly one required |
| (none) | `template` (`PoolTemplateSpec`, optional, oneof with `templateRef`) | NEW inline template (see table above) |
| `replicas` | `warm.min` / `snapshots.replicasPerNode` (see note) | v2 splits warm-pod sizing (`warm`) from snapshot fan-out (`snapshots`); v1 `replicas` maps to the fixed `warm.min` for back-compat, and the scale subresource stays on a `replicas`-shaped path |
| `snapshotAfter` | `snapshots` block | folded into the `snapshots` config |
| `snapshotDelay` | `snapshots` block | folded into the `snapshots` config |
| `scaleDownAfterSnapshot` | `snapshots` block | folded into the `snapshots` config |
| `snapshotStorage` | `snapshots` block | folded into the `snapshots` config |
| `drainPolicy` (`Kill\|Checkpoint`) | `drainPolicy` | unchanged; husk drain behavior |
| `autoscale` (`PoolAutoscaleSpec`) | `warm` (min/max/targetPending) | the v2 `warm` block is the autoscaler; v1 `autoscale.minWarm/maxWarm/targetSpare/scaleDownCooldownSeconds` map onto `warm.min/warm.max/warm.targetPending` and a cooldown field |
| `placement` (`PoolPlacement`) | `placement` | unchanged; dedicated-node pinning (#172) |
| `SandboxPoolStatus` (all fields) | `SandboxPoolStatus` | unchanged: readySnapshots, totalSnapshots, restoringCount, lastSnapshotTime, conditions, nodeDistribution, templateDigest, desiredWarm, lastScaleDownTime |

The `warm`/`snapshots` regrouping is a presentation change in the v2 spec; the
conversion preserves every v1 value (no warm-pool or snapshot setting is lost),
it only re-homes the field paths. The exact `warm`/`snapshots` field names are
fixed when the v2 pool type is written; this table fixes the source-to-target
mapping so no v1 setting is dropped.

## SandboxClaim + SandboxFork -> Sandbox

The two run-axis kinds fold into one `Sandbox`. `source` is a required oneof
selecting the origin; `replicas` carries the fan-out.

### source (the discriminated union)

| v1alpha1 | v2 `Sandbox.source` | Notes |
| --- | --- | --- |
| `SandboxClaim.poolRef` | `source.poolRef` | a fresh sandbox from a pool snapshot (the old claim); `replicas: 1` |
| `SandboxFork.sourceRef` | `source.fromSandbox` | a fork of a live sandbox; `replicas: N` for fan-out |
| (none) | `source.fromRevision` (`{workspace, revision}`) | NEW: lineage resume from a workspace revision; no v1 source |
| `SandboxFork.replicas` | `Sandbox.replicas` (default 1) | `1` = single sandbox, `>1` with `fromSandbox` = indexed sibling children |

Exactly one of `poolRef`, `fromSandbox`, `fromRevision` is set. A v1
`SandboxClaim` converts to `source.poolRef` + `replicas: 1`; a v1 `SandboxFork`
converts to `source.fromSandbox` + `replicas: <its replicas>`.

### SandboxClaimSpec fields

| v1alpha1 `SandboxClaimSpec` | v2 `Sandbox.spec` | Notes |
| --- | --- | --- |
| `poolRef` | `source.poolRef` | see source table |
| `env` | `env` | unchanged (`[]corev1.EnvVar`) |
| `secrets` (`[]SecretMount`) | `secrets` | unchanged shape |
| `volumeOverrides` (`[]VolumeOverride`) | `volumeOverrides` | unchanged |
| `timeout` (`*metav1.Duration`) | `lifetime.ttl` | re-homed under `lifetime` |
| `idleTimeout` (`*metav1.Duration`) | `lifetime.idleTimeout` | re-homed under `lifetime` |
| `nodeName` | `nodeName` | unchanged; node preference |
| `workspaceRef` (`*LocalObjectReference`) | `workspaceRef` | unchanged; single-writer workspace binding |
| `serviceAccount` | `serviceAccount` | unchanged; principal for grants and memory-snapshot binding |
| `checkpointOnTerminate` (`bool`) | `lifetime.onTerminate.snapshot` (`retain-last-N`) | generalized: the boolean becomes a retention directive |
| `ttlSecondsAfterFinished` (`*int32`) | `lifetime.ttlSecondsAfterFinished` | unchanged meaning; finished-sandbox etcd TTL, re-homed under `lifetime` |
| `outputs` (`[]OutputSpec`) | `lifetime.onTerminate.outputs` | unchanged `OutputSpec` shape (path, diff, git); re-homed under `lifetime.onTerminate` |
| (none) | `resume` (`memory\|filesystem`) | NEW: warm-memory vs filesystem-only restore for `fromSandbox`/`fromRevision`; default `memory`; cross-principal handoff forces `filesystem` |
| (none) | `budget` (`SandboxBudget`) | NEW: capability budget (maxForks, maxCheckpoints, maxCpuSeconds, maxLifetimeExtension, maxEgressBytes); defaults from pool `defaultBudget`; runtime enforcement is #25 |
| (none) | `network.extraAllow` | NEW per-sandbox egress additions on top of the pool network policy; default empty |
| (none) | `secretInheritance` (`reissue\|inherit`) | for a `poolRef` source this is always `reissue` (no parent secrets to inherit); see the fork table for the `fromSandbox` mapping |

`OutputSpec` (`path`, `diff`, `git` with `GitOutput`) and `GitOutput` (`remote`,
`branch`) carry across UNCHANGED; they already match the v2 spec
`onTerminate.outputs` shape. Only their parent path moves from
`SandboxClaim.outputs` to `Sandbox.lifetime.onTerminate.outputs`.

### SandboxForkSpec fields

| v1alpha1 `SandboxForkSpec` | v2 `Sandbox.spec` | Notes |
| --- | --- | --- |
| `sourceRef` | `source.fromSandbox` | see source table |
| `replicas` | `replicas` | the fan-out count |
| `volumeOverrides` (`[]VolumeOverride`) | `volumeOverrides` | unchanged |
| `pauseSource` (`bool`) | `source.fromSandbox.pauseSource` (or a fork option) | unchanged meaning; pause the source during checkpoint |
| `allowSecretInheritance` (`bool`) | `secretInheritance` (`reissue\|inherit`) | RENAMED and inverted to a safer default: `false` -> `reissue` (default), `true` -> `inherit`. `inherit` requires source opt-in; see docs/fork-correctness.md section 3 |

### status: SandboxClaimStatus + SandboxForkStatus -> SandboxStatus

| v1alpha1 | v2 `Sandbox.status` | Notes |
| --- | --- | --- |
| `SandboxClaimStatus.phase` (`SandboxPhase`) | `phase` | v2 phases: Pending, Hydrating, Ready, Terminating, NodeLost, Failed; v1 `Restoring` maps to `Hydrating`, v1 `Terminated` is a terminal phase reaped by TTL |
| `SandboxClaimStatus.endpoint` | `endpoint` | unchanged |
| `SandboxClaimStatus.node` | (folded into `pod`) | the husk pod name (`status.pod`) is the operator-visible handle; node is derivable from the pod |
| `SandboxClaimStatus.sandboxID` | `sandboxID` | unchanged |
| `SandboxClaimStatus.forkTimeMicros` | `startupLatencyMs` | renamed and rescaled (micros -> ms) to the v2 spec field |
| `SandboxClaimStatus.startedAt` | `startedAt` | unchanged |
| `SandboxClaimStatus.finishedAt` | `finishedAt` | unchanged; drives the GC TTL pass |
| `SandboxClaimStatus.conditions` | `conditions` | unchanged; typed conditions with observedGeneration |
| (none) | `pod` | NEW explicit husk pod name (`heartbeat-7f3a-husk`), visible to kubectl, quotas, OpenCost |
| (none) | `revision` | NEW: the WorkspaceRevision produced on terminate |
| (none) | `budgetSpend` | NEW: `{forks, cpuSeconds, ...}` capability-budget accounting (#25) |
| `SandboxForkStatus.readyForks` | `readyReplicas` | the ready child count for a `replicas > 1` Sandbox |
| `SandboxForkStatus.totalForks` | `replicas` (desired) / a status total | the total child count |
| `SandboxForkStatus.forks` (`[]ForkInfo`) | `children` (per-replica status) | the per-child list (name, sandboxID, endpoint, node, phase) for a fan-out Sandbox |
| `SandboxForkStatus.forkSnapshotTaken` (`bool`) | internal guard, retained in status | the exactly-once fork-snapshot guard; carries across as a status field so the controller-restart correctness holds |
| `SandboxForkStatus.checkpointTime` (`*metav1.Time`) | `checkpointTime` | unchanged |

## Shared types

| v1alpha1 | v2 | Notes |
| --- | --- | --- |
| `LocalObjectReference` (`{name}`) | unchanged | shared reference type |
| `SecretMount` (`name, secretRef, envVar, mountPath`) | unchanged | |
| `VolumeOverride` (`name, forkPolicy`) | unchanged | |
| `ForkPolicy` (`Fresh\|Share\|Clone\|Snapshot`) | unchanged | |
| `NetworkPolicy` (`egress, allow`) | unchanged; `extraAllow` is the per-sandbox additive form | |
| `OutputSpec`, `GitOutput` | unchanged | already v2-spec-shaped |
| `SandboxPhase` | extended | adds `Hydrating`, `NodeLost`; `Restoring` -> `Hydrating` on convert |

## Workspace and WorkspaceRevision: unchanged

`Workspace` and `WorkspaceRevision` (`api/v1alpha1/workspace_types.go`) already
match the v2 target (ADR 0002). No field moves. The Sandbox-to-Workspace edges
(`Sandbox.workspaceRef`, the terminate-produced `status.revision`, the
`RevisionSource.fromClaim` lineage) re-point from "claim" to "sandbox" naming in
prose, but the `WorkspaceRevisionSpec.source.fromClaim` field keeps its name for
storage compatibility (it names the producing Sandbox; a later optional rename to
`fromSandbox` would itself be a conversion-table row, not done here).

## Conversion mechanics

Per ADR 0007 the migration is the standard multi-version CRD path: serve both
versions, a conversion webhook translates per the tables above, a storage
migration walks existing objects, then the old version is set `served: false`
and removed. The conversion is total: every v1 field has a v2 destination here,
every removed kind (`SandboxTemplate`, `SandboxFork`) has a surviving host
(`SandboxPool`, `Sandbox`), and every v2-only field (`budget`, `resume`,
`fromRevision`, `pod`, `revision`, `budgetSpend`) has a documented default so a
v1-to-v2 conversion never requires a value the operator must invent. No v1
information is lost in conversion.

The conditions reason-code catalogue (docs/conditions.md) re-homes with the
kinds: the `SandboxFork` reasons (`SecretInheritanceDenied`, `ExplicitOptIn`,
`Forked`/`ForksCreated`) become `Sandbox` reasons on a `source.fromSandbox`
Sandbox; the `SandboxClaim` reasons become `Sandbox` reasons unchanged. Renaming
a reason code is itself an API change (docs/conditions.md), so the migration
keeps the existing reason strings and only changes which kind emits them.
