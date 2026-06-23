# Docs sweep A: mitos.run/v1 re-key report

## Summary

All 14 in-scope files were updated to describe `mitos.run/v1` as the current API.
The removed kinds (`SandboxTemplate`, `SandboxClaim`, `SandboxFork`) no longer
appear as current-API nouns in any target file.

## Per-file changes

### README.md
- `spec.outputs` -> `spec.lifetime.onTerminate.outputs` in the durable-state table.
- `live SandboxFork` -> `live sandbox fork` in the project-status prose (lowercase, not a kind name).

### ROADMAP.md
- W4 declarative foundation: `mitos.run/v1alpha1` -> `mitos.run/v1` for Workspace/WorkspaceRevision CRDs.
- W4 slice 2: `SandboxClaim.spec.workspaceRef` -> `Sandbox.spec.workspaceRef`; `fromClaim` lineage -> `fromSandbox`.
- W4 slice 3: `claim spec.outputs` -> `sandbox spec.lifetime.onTerminate.outputs`.
- Foundations (#31): `<template>-enc-key` Secret owner-referenced to `SandboxTemplate` -> `<pool>-enc-key` Secret owner-referenced to `SandboxPool`.
- Section 1: `allowSecretInheritance: true` -> `secretInheritance: inherit`.
- W2 facade slice 1: bridged `SandboxClaim` -> bridged `Sandbox`.
- Section 7 CLI: `SandboxClaim path` -> `Sandbox path`.
- Section 7 kubectl plugin: `ls (SandboxClaims), ps (SandboxForks)` -> `ls (Sandboxes), ps (fork fan-outs)`; `claim's <claim>-sandbox-token` -> `sandbox's <name>-sandbox-token`.
- Section 7 SDK ergonomics: `SandboxTemplate plus a SandboxPool` -> `SandboxPool with an inline template`.

### BENCHMARKS.md
- `SandboxClaim`s in activate-latency script description -> `Sandbox`es.
- Facade table: bridged `SandboxClaim` -> bridged `Sandbox`.
- Claim-first-exec harness description: `SandboxClaim` -> `Sandbox`.

### docs/api/v2-spec.md
No changes. The two mentions of `SandboxTemplate`/`SandboxFork` on line 207 are
explicit past-tense historical narrative ("The former `SandboxTemplate` is
inlined...the former `SandboxFork` is folded..."), correctly framing them as
prior v1alpha1 kinds. Line 209 correctly states the consolidation has landed.
Kept as-is per the judgment rule.

### docs/api/capability-budgets.md
- `api/v1alpha1` -> `api/v1` for Go type location.
- Rewrote "Where the types land (ADR 0007)" section to reflect the landed
  consolidation: the four v1alpha1 kinds are named as historical context
  ("the former v1alpha1 kinds...have been consolidated"), and the current state
  is `Sandbox` in `mitos.run/v1`.
- `SandboxFork.spec.allowSecretInheritance` gate -> `Sandbox.spec.secretInheritance` field.

### docs/api/errors.md
- Cross-reference table: `SandboxClaim.status.conditions` -> `Sandbox.status.conditions`.
- Cross-reference table: `SandboxFork.status.conditions` -> `Sandbox.status.conditions`.

### sdk/python/README.md
- Mode description: `SandboxClaim, SandboxFork, SandboxPool, SandboxTemplate` CRDs -> `Sandbox, SandboxPool` in the `mitos.run/v1` API group.
- Default pool: `SandboxTemplate carrying the image plus a SandboxPool` -> `SandboxPool with an inline template`.
- Template builder: `SandboxTemplate spec` -> `SandboxPool spec with an inline template`.

### sdk/typescript/README.md
- Mode description: `SandboxClaim, SandboxFork` in `mitos.run/v1alpha1` -> `Sandbox, SandboxPool` in `mitos.run/v1`.
- Comment: `Blocks until the SandboxClaim is Ready` -> `Blocks until the Sandbox is Ready`.
- Comment: `Terminate deletes the SandboxClaim` -> `Terminate deletes the Sandbox`.
- Default-pool comment: `SandboxTemplate plus a SandboxPool` -> `SandboxPool with an inline template`.
- Parity table: `creates a SandboxClaim` -> `creates a Sandbox`; `lists SandboxClaims` -> `lists Sandboxes`.

### sdk/go/README.md
- Scope section: old four-kind list -> `mitos.run/v1` CRDs: `Sandbox, SandboxPool, Workspace, WorkspaceRevision`.
- Deferred section: `Kubernetes / cluster mode (controller, forkd, CRDs)` -> `mitos.run/v1 CRDs`.

### sdk/ruby/README.md
- Scope section: same four-kind list -> `mitos.run/v1` CRDs.
- Deferred section: same update.

### sdk/rust/README.md
- Scope section: same four-kind list -> `mitos.run/v1` CRDs.
- Deferred section: same update.

### sdk/java/README.md
- Scope section: same four-kind list -> `mitos.run/v1` CRDs.
- Deferred section: same update.

### bench/facade/README.md
- Two references to our bridged `SandboxClaim` -> bridged `Sandbox`.
- The `agents.x-k8s.io/v1alpha1` reference on line 16 is the UPSTREAM SIG API group and was NOT changed.

### deploy/dev/README.md
- `SandboxTemplate + SandboxPool (dev-default)` -> `SandboxPool (dev-default, with an inline template)`.
- `claims` -> `sandboxes` in the namespacing sentence.

## Historical mentions kept (with reason)

| File | Location | Content kept | Reason |
| --- | --- | --- | --- |
| docs/api/v2-spec.md | line 207 | "The former `SandboxTemplate`...the former `SandboxFork`..." | Explicit past-tense historical narrative; the sentence exists to explain the consolidation. Already reads as history. |
| docs/api/v2-spec.md | line 209 | "four v1alpha1 kinds to three v1 nouns...v1alpha1 and v1alpha2 are removed" | Correct migration context, past tense, states the current state. |
| docs/api/capability-budgets.md | lines 59-61 | "the former v1alpha1 kinds...have been consolidated" | My own rewrite: correctly past-tense historical framing. |
| ROADMAP.md | lines 81, 96 | `SandboxClaim` in W1 done-slice implementation detail | These describe internal controller code paths (the reconciler that was renamed); implementation-level history of completed work. |
| ROADMAP.md | line 164 | `SandboxFork` in W1 open-item | References `internal/controller/sandboxfork_controller.go` (actual file name); implementation-level. |
| ROADMAP.md | lines 216, 234, 238, 242-243 | `SandboxTemplate`, `SandboxClaim` in W2 facade mapping | These are the UPSTREAM `extensions.agents.x-k8s.io` kinds (their API, not ours) being mapped to our engine. Must not be changed. |
| bench/facade/README.md | line 16 | `agents.x-k8s.io/v1alpha1` | The upstream SIG API group. Out of scope per task instructions. |

## Gate: grep results

```
grep -rn "mitos.run/v1alpha1|mitos.run/v1alpha2|kind: SandboxClaim|kind: SandboxFork|kind: SandboxTemplate" ...
```

Output: empty. All explicit group-version and YAML kind headers are removed from target files.

## Files changed

1. README.md
2. ROADMAP.md
3. BENCHMARKS.md
4. docs/api/capability-budgets.md
5. docs/api/errors.md
6. sdk/python/README.md
7. sdk/typescript/README.md
8. sdk/go/README.md
9. sdk/ruby/README.md
10. sdk/rust/README.md
11. sdk/java/README.md
12. bench/facade/README.md
13. deploy/dev/README.md

docs/api/v2-spec.md: no changes (all mentions already correctly historical).

## Concerns

None. The YAML manifest examples in README.md were already in `mitos.run/v1` shape
before this sweep. The CRD schemas in `deploy/crds/` confirm the v1 field names
used throughout. No benchmark numbers were altered.
