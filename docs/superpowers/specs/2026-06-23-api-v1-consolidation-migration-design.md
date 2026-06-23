# Design: migrate mitos.run to a stable v1, consolidated to three nouns

Date: 2026-06-23
Issue: #23 (API v2 consolidate CRDs to three nouns), #22 (API v2 epic)
Related: docs/adr/0007-api-v2-three-noun-consolidation.md,
docs/adr/0001-facade-and-naming.md, docs/api/v2-migration.md,
docs/api/v2-spec.md, docs/conditions.md

## Goal

Discharge the single breaking API migration that ADR 0001 deferred and ADR 0007
designed, so the 1.0.0 release ships a stable, honest CRD surface. After this
work the cluster serves exactly one group version, `mitos.run/v1`, with three
declarative nouns (`SandboxPool`, `Sandbox`, `Workspace`) plus the
`WorkspaceRevision` companion. The four v1alpha1 kinds (`SandboxTemplate`,
`SandboxPool`, `SandboxClaim`, `SandboxFork`) are removed.

## Decisions taken (constraints for this work)

1. Target version is `mitos.run/v1` (stable), not v1alpha2 or v1beta1. The
   existing `api/v1alpha2` types are the shape; they are promoted to `v1`.
2. Clean break: `api/v1alpha1` and its CRDs are deleted outright. No conversion
   webhook, no serve-both transition, single served version equals stored
   version. Justification: there is no existing usage to preserve, and
   docs/api/v2-migration.md remains the field-by-field record for anyone holding
   old manifests.
3. Atomic scope: control plane, facade, kubectl-mitos, MCP, CLI tables,
   manifests, examples, docs, and the Python + TypeScript SDK cluster mode all
   cut over together so nothing is left speaking the removed kinds.
4. `source.fromRevision` ships declared in the `v1` schema but not served: the
   reconciler reports a clear not-ready condition for it. The lineage-resume
   engine path is sequenced after this migration (issue #25 area). It is never
   silently dropped.

## Non-goals

- gRPC Connect wire migration (issue #24): out of scope, JSON transports stay.
- Runtime budget enforcement (issue #25): the `budget` field carries across and
  defaults from the pool, but enforcement is not wired here.
- The `fromRevision` engine path: declared, not served (see decision 4).

## Target API surface: mitos.run/v1

The `api/v1alpha2` package is renamed to `api/v1` with group version
`mitos.run/v1`. `api/v1alpha1` is deleted. Served kinds:

- `SandboxPool`: inline `template` (a `PoolTemplateSpec` absorbing every field
  of the old `SandboxTemplateSpec`: image, init, command, env, resources,
  volumes, network, encrypted) OR an optional `templateRef` (mutually
  exclusive); plus `snapshots`, `warm`, `placement`, `defaultBudget`.
- `Sandbox`: one running sandbox. `source` is a discriminated union of exactly
  one of `poolRef` (was SandboxClaim.poolRef), `fromSandbox` (was
  SandboxFork.sourceRef), or `fromRevision` (new, declared not-served).
  `replicas` (default 1) folds the fork fan-out into the same kind. Carries
  `resume`, `budget`, `lifetime`, `secretInheritance`, `workspaceRef`, `env`,
  `secrets`, `serviceAccount`, `network.extraAllow`, `volumeOverrides` per the
  ADR 0007 field map. `status` consolidates claim and fork status, including the
  per-child list when `replicas > 1`.
- `Workspace` and `WorkspaceRevision`: moved from v1alpha1 to v1 unchanged in
  shape (ADR 0002 already shipped them in the target form).

All conversion machinery is removed: `conversion.go`, the `Convertible`
implementations, the Hub designation, and `webhook.go` conversion wiring in the
v1alpha2 package are deleted, since there is no second version to convert to.

## Engine absorption (the crux, highest risk)

Today the v1alpha2 `SandboxReconciler` is a thin delegating reconciler: it
creates child `SandboxClaim` / `SandboxFork` objects and mirrors their status.
The real engine logic lives in `sandboxclaim_controller.go` (~1350 lines),
`sandboxfork_controller.go`, and the v1alpha1 `sandboxpool_controller.go`. A
clean break requires the v1 reconcilers to own the engine directly.

- The `v1.Sandbox` reconciler absorbs the `sandboxclaim_controller.go` engine on
  the `source.poolRef` branch (the claim path: node selection from the
  NodeRegistry, forkd `Fork` over gRPC, status endpoint, secrets delivery,
  bearer token issue, lifetime/idle, terminate-with-outputs) and the
  `sandboxfork_controller.go` engine on the `source.fromSandbox` branch (live
  fork, `replicas: N`, per-child status, secret inheritance policy).
- The `v1.SandboxPool` reconciler absorbs the `SandboxTemplate` inline-template
  handling (snapshot build inputs) into the existing v1alpha2 pool reconciler.
- Shared helpers (`gc.go`, `scheduler.go`, `forkd_client.go`, `finalizer.go`,
  secret replication, husk* lifecycle, metrics, eventfeed) are re-keyed from the
  removed types to `v1.Sandbox` / `v1.SandboxPool`.
- `sandboxclaim_controller.go`, `sandboxfork_controller.go`, and the v1alpha1
  `sandboxpool_controller.go` are deleted once their logic is rehomed.

The conditions catalogue re-homes per ADR 0007: the SandboxFork reasons
(`SecretInheritanceDenied`, `ExplicitOptIn`, `Forked` / `ForksCreated`) become
`Sandbox` reasons gated on `source.fromSandbox` with `replicas > 1`.

This stage moves security-load-bearing reconciler logic (secret delivery,
per-fork token issue, fork-correctness handshake triggers). It carries the most
test coverage and the closest review.

## Surface cutover (atomic)

- Facade (`internal/facade`): re-target the upstream `agents.x-k8s.io Sandbox`
  onto `v1.Sandbox{source.poolRef}` instead of `SandboxClaim`. Remove the claim,
  template, and warmpool facade reconcilers that mapped to removed kinds; keep
  the single Sandbox-to-Sandbox mapping ADR 0001 anticipated.
- `cmd/kubectl-mitos`, `internal/mcp`, `internal/cli/sandboxtable`,
  `internal/agentcli`: switch reads and writes to `v1.Sandbox` / `SandboxPool`.
- SDKs: Python and TypeScript cluster mode re-pointed to the `v1` kinds
  (`Sandbox` with `source`, inline-template pools). Direct mode (sandbox-server
  and hosted mitos.run) is untouched. Go, Ruby, Rust, Java are direct-only and
  unaffected.
- Manifests: regenerate `deploy/crds/` to `mitos.run_sandboxpools.yaml`,
  `mitos.run_sandboxes.yaml`, `mitos.run_workspaces.yaml`,
  `mitos.run_workspacerevisions.yaml`. Delete `mitos.run_sandboxtemplates.yaml`
  and `mitos.run_sandboxforks.yaml`. Update `config/rbac`, `deploy/rbac`,
  `deploy/kustomization.yaml`, and kind/talos config that names the old kinds.
- Examples and docs: rewrite `examples/claim.yaml`, `examples/fork.yaml`,
  `examples/python-pool.yaml`, `examples/multi-workspace.yaml` into `Sandbox` /
  inline-template `SandboxPool` form. Update CLAUDE.md's CRD list, README API
  references, docs/api/v2-spec.md status, and the ADR 0007 status section to mark
  the breaking migration done.

## Testing

- TDD per behavior change; every behavior lands with its test in the same commit.
- The envtest controller suite is the safety net. The existing conformance tests
  (`Sandbox{source.poolRef}` reaches Ready as the claim equivalent;
  `Sandbox{source.fromSandbox, replicas:N}` produces N children as the fork
  equivalent, mock engine) must stay green after the engine moves under them.
- Migrated GC, TTL, finalizer, secret-inheritance, and orphan-condition tests
  re-keyed to `v1`.
- A test asserting `source.fromRevision` yields the documented not-ready
  condition (declared, not served).
- Both lint invocations (`golangci-lint run` and `GOOS=linux golangci-lint
  run`) clean, plus `GOOS=linux GOARCH=amd64 go build ./guest/agent/`.
- kind-e2e mock path exercises the full cutover end to end.
- Python and TypeScript SDK cluster-mode tests updated and green.

## Execution strategy

Run in a git worktree, isolated from the current branch. Staged, each stage
landing with its tests green before the next:

1. Rename `api/v1alpha2` to `api/v1`; move Workspace and WorkspaceRevision into
   v1; delete `api/v1alpha1`; remove conversion machinery; regenerate deepcopy.
2. Absorb the claim engine into the `Sandbox` `source.poolRef` branch.
3. Absorb the fork engine into the `Sandbox` `source.fromSandbox` branch.
4. Absorb the template handling into the `SandboxPool` reconciler.
5. Facade, kubectl-mitos, MCP, CLI tables, agentcli cutover.
6. Manifests, RBAC, examples, docs, CLAUDE.md, ADR 0007 status.
7. Python and TypeScript SDK cluster mode cutover.

Drive one subagent per stage. Stages 2 to 4 (engine absorption) get the closest
review because a regression there is a fork-correctness or secret-handling
hazard, not just a build break. Manifests regenerate via `make generate
manifests` after the api changes land.

## Risks

- Engine absorption regresses a security property (secret delivery, per-fork
  token freshness, secret-inheritance default). Mitigation: the re-keyed secret
  and fork-correctness tests must pass unchanged in meaning; do not weaken any
  default during the move.
- Hidden references to removed kinds in test fixtures or third_party cause build
  breaks late. Mitigation: a full repo grep for the removed kind names is part
  of stage 6 acceptance.
- SDK cluster-mode drift if the Go-side kind shape and the SDK serialization
  disagree. Mitigation: the SDK cluster-mode tests run against the regenerated
  CRDs.
