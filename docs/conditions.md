# Conditions and reason-code catalogue

This is the NORMATIVE catalogue of the typed conditions and their reason codes
across the mitos.run CRDs. It is a document, not a wiki page: a reason code
is part of the API contract, and a change here is an API change. Tooling, the
SDK, and dashboards key off these reasons; do not rename one without a
deprecation note.

Every reconciler sets a `Ready` condition (type `Ready`) with `status`
(`True`/`False`), an `observedGeneration` matching the object's `generation`,
and one of the reason codes below. Condition `message` is human/LLM-legible and
carries remediation; it is not part of the contract and may change.

## Workspace (`mitos.run/v1`)

Condition type `Ready`. The reconciler computes `status.head` (the latest
committed revision, ordered by creationTimestamp then name),
`status.revisions` (the committed revision count), and `status.resumable` (the
head pairs with a memory snapshot).

| Reason | Status | Meaning |
| --- | --- | --- |
| `WorkspaceReady` | True | The model is valid: every revision's lineage resolves and head/revisions/resumable are computed. |
| `WorkspacePending` | False | No committed revision yet (the workspace has no head). |
| `WorkspaceDegraded` | False | A revision has a broken `fromWorkspaceRevision` lineage edge (a parent that does not resolve to a revision in the same workspace). |

## WorkspaceRevision (`mitos.run/v1`)

Condition type `Ready`, mirrored by `status.phase` (`Pending`/`Committed`). A
revision commits when its `contentManifest` is a valid content-addressed digest;
once committed it is immutable (single-writer-per-revision).

| Reason | Status | Phase | Meaning |
| --- | --- | --- | --- |
| `RevisionCommitted` | True | `Committed` | `contentManifest` is a valid content-addressed digest; the revision is frozen. |
| `RevisionPending` | False | `Pending` | Awaiting a valid `contentManifest` from dehydrate, or the revision's lineage edge does not resolve. |

## Sandbox, SandboxPool (`mitos.run/v1`)

Existing reason codes, recorded here so the catalogue is complete. See the
respective reconcilers in `internal/controller` for the precise emission points.

| Reason | Kind(s) | Meaning |
| --- | --- | --- |
| `SnapshotsReady` | SandboxPool | The pool's template snapshot is built on the desired number of holder nodes. |
| `HuskPodsReady` | SandboxPool | The warm husk pod pool is at the desired replica count with at least one snapshot node. |
| `Built` | SandboxPool (`TemplateBuilt`) | `ensureTemplateBuilt` succeeded this reconcile: the pool's template snapshot build (or rebuild) completed without error. |
| `BuildFailed` | SandboxPool (`TemplateBuilt`) | `ensureTemplateBuilt` returned an error this reconcile; the condition message carries the error (truncated to 512 characters). Covers both the husk-pod and raw-forkd reconcile paths (#578). |
| `Healthy` | SandboxPool (`TemplateHealthy`) | The warm husk pods are ready and no dormant husk pod on the current template digest is crashlooping (#584). |
| `RestoreFailing` | SandboxPool (`TemplateHealthy`) | Two or more dormant husk pods bound to the current template digest are in CrashLoopBackOff; the template is presumed restore-broken. A rebuild has not (yet) been triggered this reconcile because it is inside the backoff window (see `docs/husk-pods.md`). |
| `Rebuilding` | SandboxPool (`TemplateHealthy`) | The automatic backoff-bounded rebuild was just triggered for a restore-failing template. |
| `ForceRebuilding` | SandboxPool (`TemplateHealthy`) | The operator-triggered `mitos.run/force-rebuild` annotation just triggered an immediate rebuild, bypassing the backoff. |
| `HuskActivated` | Sandbox (source.poolRef) | A dormant husk pod was activated in place for the sandbox. |
| `ActivateFailed` | Sandbox (source.poolRef) | Activating a husk pod failed; the sandbox re-pends. |
| `HuskPodRaced` | Sandbox (source.poolRef) | Two sandboxes raced for the same dormant husk pod; this one lost and retries. |
| `NoHuskPod` | Sandbox (source.poolRef) | No dormant husk pod was available to activate. |
| `NoCapacity` / `CapacityExhausted` | Sandbox (source.poolRef) | No node had capacity to admit the sandbox before the pending deadline. |
| `PoolNotFound` | Sandbox (source.poolRef) | The referenced SandboxPool does not exist. The sandbox pends and retries for a bounded grace period (the same `--max-pending-duration` bound as the capacity wait, default 5m; the pool may be applied moments after the sandbox), then fails terminally: no further steady-state requeues, `FinishedAt` stamped so the GC TTL pass reaps it like every other Failed sandbox. |
| `NodeLost` | Sandbox (source.poolRef) | The node backing an active sandbox was lost (drain, eviction, deletion), and the sandbox failed closed: no surviving node holds the pool template snapshot to re-fork onto, or the bounded auto re-fork retries were exhausted. Terminal. |
| `NodeLostReforking` | Sandbox (source.poolRef) | The raw-forkd node backing an active sandbox was lost, but a surviving node holds the pool template snapshot, so the sandbox is being automatically re-forked onto it (issue #372). The sandbox is re-pended and the claim reconciler re-issues the fork on its normal placement path. Bounded by a per-claim retry count; exhausting it fails closed with `NodeLost`. |
| `OrphanReaped` | Sandbox (source.poolRef) | The GC orphan sweep reaped a backing VM that lingered past this (terminal) sandbox's transition, e.g. a terminate that crashed or was missed and was then re-adopted by a restarted forkd. Informational; the VM is gone. |
| `SecretInheritanceDenied` | Sandbox (source.fromSandbox) | A fork was rejected because the source sandbox holds secrets and inheritance was not explicitly opted into. |
| `ExplicitOptIn` | Sandbox (source.fromSandbox) | Secret inheritance was explicitly permitted on the fork. |
| `Forked` / `ForksCreated` | Sandbox (source.fromSandbox) | The requested forks were created. |
| `BudgetExhausted` | Sandbox (source.fromSandbox) | A self-initiated fork was rejected because the source sandbox's capability budget (`spec.budget.maxForks`) is spent; admitted forks are ranked deterministically by creation time, and the ones beyond the limit fail terminally with the LLM-legible `budget_exhausted` remediation (request a larger budget from the creator). |
| `SourceTerminated` / `SourceFailed` / `SourceGone` | Sandbox (source.fromSandbox) | The fork's source sandbox reached a terminal phase (`Terminated` or `Failed`) or its object no longer exists, so the FAN-OUT can never complete: a live fork copies the source VM's running memory, which is gone. Terminal for the fan-out (issue #698): the reason appears on a True `SourceTerminated` condition and is mirrored on `Ready=False` (the message carries the remediation: fork a Ready sandbox instead), the fork's never-activated pending child pods are deleted so their `mitos.run/kvm` and memory requests return to the scheduler, and the fork is never requeued. Already-activated children are independent sandboxes and are NEVER touched: with survivors the fork keeps their `status.children` entries and `readyReplicas` and stays in its non-terminal fan-out phase (no `finishedAt`, so the GC TTL pass cannot reap the fork object out from under its running children); with no survivors the fork moves to phase `Failed` with `finishedAt` stamped and is GC TTL-reaped like every other failed sandbox. A source that is merely not yet Ready keeps the fork waiting; only a terminal phase or a missing object triggers this. See `docs/lifecycle.md`. |
| `RevisionResumeNotImplemented` | Sandbox (source.fromRevision) | A `source.fromRevision` sandbox is declared in the v1 schema but its lineage-resume engine path is not yet served. The reconciler reports `Ready=False` with this reason and phase `Pending`, never silently dropping the sandbox; use `source.poolRef` or `source.fromSandbox` until the resume path lands. |
| `WorkspaceBusy` | Sandbox (source.poolRef) | Another writer holds the single-writer-per-workspace lock for the sandbox's target workspace; this sandbox waits and retries until the first writer releases it. |
| `CheckpointNotImplemented` | Sandbox (source.poolRef) | A pool set `DrainPolicy: Checkpoint`, but its active sandbox lost its backing husk pod and no live-VM checkpoint engine captured the state (the only state today), so the claim re-pended with Kill semantics: in-VM state was NOT captured. Surfaced loudly (this distinct reason plus a `Warning` event carrying the same reason) so a Checkpoint pool never silently degrades to Kill. The claim condition is transient (a later reconcile may set `NoHuskPod`), so the `Warning` event is the durable operator-visible signal. Full live-VM checkpoint is a tracked follow-up requiring KVM. |

After the v1 consolidation (ADR 0007) the former `SandboxClaim` and `SandboxFork`
reasons above are emitted on the single `Sandbox` kind: the claim reasons on a
`source.poolRef` Sandbox, and the fork reasons (`SecretInheritanceDenied`,
`ExplicitOptIn`, `Forked`/`ForksCreated`) on a `source.fromSandbox` Sandbox. The
reason strings are unchanged; only the kind that emits them moved.

### Operator actions per Sandbox reason

The `Ready=False` Sandbox reasons above are not all the same severity. The
catalogue is the normative reference the alerts and runbooks cite (see
`deploy/monitoring/` and `docs/runbooks/`).

| Reason | Status | Operator action |
| --- | --- | --- |
| `HuskActivated` | True | None; a dormant husk pod was activated in place. |
| `ActivateFailed` | False | Transient; the sandbox re-pends. If sustained, check forkd and KVM health on the holder node (the ClaimErrorRateHigh `reason="fork"` runbook). |
| `HuskPodRaced` | False | None; two sandboxes raced for one husk pod, the loser retries. Benign under load. |
| `NoHuskPod` | False | Warm pool is empty for this sandbox's pool; scale the SandboxPool warm count (the WarmPoolStarved runbook). |
| `NoCapacity` / `CapacityExhausted` | False | No node had admission capacity before the pending deadline; add capacity or scale pools (the ClaimsPendingSustained runbook). |
| `PoolNotFound` | False | The referenced SandboxPool does not exist. While the sandbox is `Pending` it is still inside the grace period: apply the pool and the sandbox proceeds on its own. Once `Failed` it is terminal: create the pool (or fix the `spec.source.poolRef` name) and recreate the sandbox, or delete the sandbox; the GC TTL reaps the Failed object either way. |
| `NodeLost` | False | Terminal. The backing node was lost and the sandbox could not be auto re-forked: no surviving node holds the pool template snapshot, or the bounded retries were exhausted. Confirm node and snapshot-holder health, then recreate the claim. |
| `NodeLostReforking` | False | None; the backing node was lost and the sandbox is being automatically re-forked onto a surviving snapshot holder. Investigate only if it recurs for one sandbox (a replacement node that keeps dying eventually fails closed with `NodeLost`). |
| `OrphanReaped` | False | None; the GC reaped a VM that outlived this terminal sandbox. Investigate only if it recurs, which would point at a forkd terminate path crashing or being missed. |
| `WorkspaceBusy` | False | None; the sandbox waits on the single-writer-per-workspace lock and retries. Investigate only if a writer never releases it. |
| `CheckpointNotImplemented` | False | The pool requested `DrainPolicy: Checkpoint`, which is not yet implemented; the claim re-pended with Kill semantics and in-VM state was lost. Set `DrainPolicy: Kill` knowingly, or persist state via a workspace, until the live-VM checkpoint engine lands (a KVM-gated follow-up). |
