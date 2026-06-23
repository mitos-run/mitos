# TypeScript SDK cluster mode: v1 cutover report (Task 7.2)

## Status

Done. All tests pass, package typechecks and builds clean.

## Body re-keys applied

### `sdk/typescript/src/k8s.ts`

- `API_VERSION`: `"v1alpha1"` -> `"v1"`
- `K8sApi` interface: removed `createTemplate` and `getTemplate` methods (SandboxTemplate kind is gone in v1; the pool carries the image inline).
- `KubeConfigApi`: removed `createTemplate` and `getTemplate` implementations. Updated all `plural: "sandboxclaims"` to `plural: "sandboxes"` in `createClaim`, `getClaim`, `deleteClaim`, `listClaims`, and `patchClaim`. Updated comment on workspace verbs from `v1alpha1` to `v1`.

### `sdk/typescript/src/client.ts`

- `API_VERSION`: `"v1alpha1"` -> `"v1"`
- `create()`: body changed from `kind: "SandboxClaim"` + `spec: { poolRef, ... }` to `kind: "Sandbox"` + `spec: { source: { poolRef: { name } }, ... }`. Field rename: `timeout` -> `lifetime.ttl` (nested under `spec.lifetime`).
- `ensureDefaultPool()`: removed the two-object creation path (SandboxTemplate + SandboxPool with templateRef). Now creates a single `SandboxPool` with `spec: { template: { image }, replicas: 1 }`, matching the Python v1 path.
- `verifyPoolImage()`: rewrote to read `pool.spec.template.image` inline instead of resolving a `templateRef` to a separate SandboxTemplate GET. No longer calls `k8s.getTemplate`.
- `list()`: reads pool from `spec.source.poolRef.name` (was `spec.poolRef.name`).
- `makeTerminator()`: patching path updated from flat `spec: { outputs, checkpointOnTerminate }` to nested `spec: { lifetime: { onTerminate: { outputs, snapshot } } }`. `checkpoint: true` now sets `snapshot: "retain-1"` per the v1 migration table.
- `forkTimeMs()`: added `startupLatencyMs` (v1 field) as the primary branch; `forkTimeMicros`/`forkTimeMs` kept as fallbacks for objects observed before storage migration completes.
- Error strings: replaced "claim" with "sandbox" in cause/remediation text; `"SandboxClaim"` -> `"Sandbox"` in remediation message.

### `sdk/typescript/src/workspace.ts`

- `API_VERSION`: `"v1alpha1"` -> `"v1"`

### `sdk/typescript/examples/cluster.ts`

- Updated docstring: removed reference to `SandboxClaim`; now describes `mitos.run/v1 Sandbox`.

### `sdk/typescript/test/client.test.ts`

- Rewrote `FakeK8s`: removed `createTemplate`, `getTemplate`, `createdTemplates`, `existingPoolImage` (via templateRef path). Added `existingPoolImage` kept but now returned inline in `spec.template.image` on `getPool`. Added stub implementations of all workspace K8sApi methods so the fake satisfies the full interface. Removed `templateThrowsStatus` option; replaced with test that relies on `existingPoolImage` being `undefined`.
- Updated expected body: `apiVersion: "mitos.run/v1"`, `kind: "Sandbox"`, `spec.source.poolRef.name`, `spec.lifetime.ttl`.
- List test: items now use `spec: { source: { poolRef: { name } } }` and `status.startupLatencyMs` instead of `forkTimeMicros`.
- Default pool test: removed assertions on `createdTemplates`; asserted `createdPools[0].spec = { template: { image: "python" }, replicas: 1 }`.
- "fails closed when pool has no inline image" test replaces the old "fails closed when template cannot be read" test; `getPool` now returns a pool with no `spec.template.image`.

## npm test + build output

```
Tests  88 passed | 1 skipped (89)    (conformance skipped, pre-existing)
Build: tsc exits 0, no errors
```

## TS-vs-Python parity gaps

The following cluster-mode capabilities exist in `sdk/python/mitos/client.py` (v1) but are absent from the TypeScript `AgentRun`:

1. **`get(name: str) -> Sandbox`** (Python line 263): reconnects to an existing sandbox by name, reading endpoint and phase from the cluster and loading the token if Ready. TypeScript has `fromName(name)` which only reads endpoint/token but does NOT read the phase or populate a SandboxInfo-level handle. Python's `get()` is the general reconnect path; `from_name()` is an alias on top of it. Both are present in Python; TypeScript only has `fromName` (which is functionally similar but does not surface the phase on the returned object).

2. **`pool_status(name: str) -> PoolStatus`** (Python line 345): fetches a `SandboxPool` and returns a `PoolStatus` (readySnapshots, desired, nodeDistribution). TypeScript has no equivalent public method. The `PoolStatus` type is defined in `sdk/typescript/src/types.ts` but is never populated by the cluster client.

3. **`secrets` parameter on `create()` / `sandbox()`** (Python lines 64, 195-238): passes `dict[str, tuple[str, str]]` (envVar -> (secretName, secretKey)) that posts `spec.secrets` as a `[]SecretMount`. TypeScript `CreateOptions` has no `secrets` field.

4. **`sandbox(ready=True)` non-blocking vs. blocking split** (Python lines 58-106): Python's `create()` does not poll; it just posts the Sandbox and returns immediately. The optional `ready=True` on `sandbox()` then calls `wait_until_ready()`. TypeScript's `create()` always polls to Ready. This means TypeScript has no way to get a non-polled handle from `create()`.

## Files changed

- `sdk/typescript/src/k8s.ts`
- `sdk/typescript/src/client.ts`
- `sdk/typescript/src/workspace.ts`
- `sdk/typescript/examples/cluster.ts`
- `sdk/typescript/test/client.test.ts`

## Self-review

- Direct mode code paths (`SandboxServer`, `http.ts`, `sandbox.ts`, `server.ts`) are unchanged.
- No em/en dashes introduced.
- Wire shapes match Python v1 exactly for `create` (Sandbox + source.poolRef), `ensureDefaultPool` (SandboxPool + inline spec.template), and `list` (source.poolRef path).
- `startupLatencyMs` fallback chain is conservative: v1 field first, then legacy fields, so objects in a partially-migrated cluster still report a non-zero latency.
- `makeTerminator` patch now nests outputs under `lifetime.onTerminate` per the v1 migration table; `checkpoint` maps to `snapshot: "retain-1"` (a reasonable v1 directive; the exact retention string should be confirmed against the v1 CRD enum when the controller schema lands).

## Concerns

- The `snapshot: "retain-1"` value in the terminator is inferred from the migration doc text (`checkpointOnTerminate (bool)` -> `lifetime.onTerminate.snapshot (retain-last-N)`). If the v1 CRD enum uses a different string (e.g., `"retain-last-1"` or an integer), this will be rejected by the controller. Needs confirmation against the actual Go type once the v1 controller schema is written.
- Parity gap 4 (always-polling `create()`) is a behavioral difference that may surprise users migrating from Python. If Python's non-polling `create()` is part of a deliberate "let the user decide when to wait" design, TypeScript should match it. Flagged for the parity workstream.
