# docs-examples-report: re-key operator-facing examples to mitos.run/v1

## Per-file changes

### docs/workspaces.md
- First YAML block (binding example): `apiVersion: mitos.run/v1alpha1`, `kind: SandboxClaim`, flat `poolRef` -> `apiVersion: mitos.run/v1`, `kind: Sandbox`, `source.poolRef`.
- Second YAML block (outputs example): same kind/version swap; `spec.outputs` moved to `spec.lifetime.onTerminate.outputs` per v2-migration.md mapping.
- Prose at line 13: "A `SandboxClaim` opts into workspace state" -> "A `Sandbox` opts into workspace state".
- Prose at line 151: "A claim narrows ... with `spec.outputs`, matching the v2-spec `onTerminate.outputs` shape" -> "A sandbox narrows ... with `spec.lifetime.onTerminate.outputs`".
- All other "claim" prose is narrative (describing lifecycle/behavior) and left intact.

### docs/platforms/talos-hetzner.md
- Section 5a CRD list: "four CRDs (`SandboxTemplate`, `SandboxPool`, `SandboxClaim`, `SandboxFork`)" -> "three CRDs (`SandboxPool`, `Sandbox`, `Workspace`)".
- Section 6 heading: "create a SandboxPool, claim, and exec" -> "create a SandboxPool, sandbox, and exec".
- Section 6a heading: "Create a SandboxTemplate and SandboxPool" -> "Create a SandboxPool"; YAML combined `SandboxTemplate` + `SandboxPool with templateRef` -> single `SandboxPool` with inline `spec.template` and `spec.warm.min: 2`; filename `sandbox-template.yaml` -> `sandboxpool.yaml`; `kubectl get sandboxpool` -> `kubectl get sandboxpools`.
- Section 6b heading: "Create a SandboxClaim" -> "Create a Sandbox"; YAML `kind: SandboxClaim`, flat `poolRef` -> `kind: Sandbox`, `source.poolRef`; filename `sandbox-claim.yaml` -> `sandbox.yaml`; `kubectl get sandboxclaim` -> `kubectl get sandboxes`.
- Section 6d cleanup: removed `delete sandboxtemplate`; `delete sandboxclaim` -> `delete sandbox`.

### docs/perf/cpu-pinning.md
- Prose line 65: api group inline reference `mitos.run/v1alpha1` -> `mitos.run/v1`.
- No YAML blocks to change. The `spec.cpuPinning` example block has no `apiVersion` line.

### docs/integrations/paperclip.md
- Line 6: "SandboxClaims" -> "Sandboxes".
- Line 17: `SandboxClaim` (`mitos.run/v1alpha1`) -> `Sandbox` (`mitos.run/v1`).
- Contract table: `SandboxClaim` -> `Sandbox`; `claim.spec.timeout` -> `sandbox.spec.lifetime.ttl`; `claim.spec.idleTimeout` -> `sandbox.spec.lifetime.idleTimeout`; updated teardown, egress, secrets rows to "sandbox" phrasing.
- Section heading "Lease lifetime to claim TTL" -> "Lease lifetime to sandbox TTL".
- `SandboxClaimSpec.Timeout/IdleTimeout` (`api/v1alpha1/types.go`) -> `SandboxSpec.Lifetime.TTL/IdleTimeout` (`api/v1/types.go`).
- `SandboxClaimSpec.Outputs` -> `SandboxSpec.Lifetime.OnTerminate.Outputs`; `deleteClaim` -> `deleteSandbox`.
- `api/v1alpha1/types.go` `NetworkPolicy` reference -> `api/v1/types.go`.
- `SandboxClaimSpec.Secrets` / `sandboxclaim_controller.go` -> `SandboxSpec.Secrets` / `sandbox_controller.go`.
- "claim-time" phrasing updated to "sandbox-time" in the secrets and per-adapter sections.

## Historical mentions kept

None. All occurrences were live examples or direct API field references, not historical narrative.

## Gate grep output

```
(no output - all targeted patterns cleared)
```

Command run:
```
grep -rn "mitos.run/v1alpha1|kind: SandboxClaim|kind: SandboxFork|kind: SandboxTemplate|sandboxclaims|sandboxforks|sandboxtemplates" docs/workspaces.md docs/platforms/talos-hetzner.md docs/perf/cpu-pinning.md docs/integrations/paperclip.md
```
Returns empty.

## Field name verification

All v1 field names verified against authoritative CRD schemas:
- `deploy/crds/mitos.run_sandboxes.yaml`: `spec.source.poolRef`, `spec.source.fromSandbox`, `spec.lifetime.ttl`, `spec.lifetime.idleTimeout`, `spec.lifetime.onTerminate.outputs`, `spec.replicas`, `spec.secretInheritance` - all confirmed present.
- `deploy/crds/mitos.run_sandboxpools.yaml`: `spec.template`, `spec.warm.min` - confirmed at lines 206 and 692/811.

## Concerns

None. All changes are mechanical re-keying per the v2-migration.md mapping table. No prose narrative rewritten beyond minimal noun changes (SandboxClaim -> Sandbox, claim -> sandbox where directly describing the API object).
