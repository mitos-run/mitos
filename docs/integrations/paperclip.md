# Paperclip / OpenClaw / Hermes integration

This document describes how Mitos serves as the execution substrate for the
Paperclip ecosystem (issue #20, workstream W3). The integration implements the
upstream pluggable sandbox-provider contract (Environments plus a lease
lifecycle) and maps it onto Mitos Sandboxes instead of `batch/v1` Jobs.

The in-repo plugin lives at `plugins/paperclip`. It is a TypeScript Paperclip
plugin (`@paperclipai/sandbox-provider-sandbox`) that registers an environment
driver (`driverKey: "sandbox"`, `kind: "sandbox_provider"`). The plugin has two
backends, selected by `config.backend`:

- `server`: the original skeleton path, talking to the standalone
  sandbox-server REST API (`/v1/fork`, `/v1/exec`, `/v1/sandboxes`). No
  Kubernetes.
- `claim`: the workstream-A target. The provider contract is realized as a
  Mitos `Sandbox` (`mitos.run/v1`) on Kubernetes.

## Production gate (read first)

Claim mode does NOT ship to production tenants until BOTH of these are green in
CI:

- **#3 fork-correctness** (hostile inputs and real credentials in forked VMs;
  see `docs/fork-correctness.md`).
- **#163 failure / GC semantics** (crash, node loss, residual GC; see
  `docs/failure-gc.md`).

Both have advanced but remain open 1.0 gates. The plugin's `claim` backend is
implemented and unit tested, but enabling it for production tenants is blocked
on those two gates. This is called out in the manifest's `backend` config
description as well.

## Provider-contract mapping (workstream A)

The contract maps onto Mitos as follows. The pure mapping is in
`plugins/paperclip/src/claim-mapping.ts`; the lifecycle orchestration is in
`plugins/paperclip/src/claim-client.ts`. Both are unit tested with no cluster
(`plugins/paperclip/test/*.test.ts`).

| Provider contract | Mitos mapping | Code |
| --- | --- | --- |
| create environment / acquire lease | create a `Sandbox` from a pool, wait Ready | `leaseToClaim`, `acquireWithAssertion` |
| lease max lifetime | `sandbox.spec.lifetime.ttl` (wall-clock cap; zero is no limit) | `minutesToDuration` |
| lease idle reaping | `sandbox.spec.lifetime.idleTimeout` | `minutesToDuration` |
| teardown | extract workspace artifacts FIRST, then delete the sandbox | `teardownWithExtract`, `terminateToOutputs` |
| callback-bridge egress | a single sandbox-time egress allow entry over default-deny | `bridgeToEgressAllow` |
| secrets (git creds, API keys, bridge token) | `sandbox.spec.secrets` SecretMounts, injected at sandbox time by reference | `secretsToClaimMounts` |
| per-adapter installs | asserted at sandbox time by a PATH probe, never installed per run | `assertAdapterInstalls` |

### Lease lifetime to sandbox TTL

`maxLifetimeMin` maps to `SandboxSpec.Lifetime.TTL` and `idleTimeoutMin` maps to
`SandboxSpec.Lifetime.IdleTimeout` (`api/v1/types.go`). Minutes are rendered as
Go duration strings (`metav1.Duration`), whole minutes as `<n>m` and fractional
minutes in seconds to avoid lossy rounding. A zero, negative, or absent value
leaves the field unset, which the controller reads as "no limit".

### Teardown: extract before delete

Teardown maps to sandbox deletion, but the workspace artifacts must be extracted
first. `teardownWithExtract` patches the terminate-with-outputs directives
(`SandboxSpec.Lifetime.OnTerminate.Outputs`) onto the sandbox, which makes the controller
dehydrate the `/workspace` into a committed `WorkspaceRevision` on the way out,
and only THEN deletes the sandbox. The ordering is the contract: the function
never reorders the two calls and never deletes if the output patch fails, so an
artifact is never lost to a premature delete. This mirrors the `@mitos/sdk`
`AgentRun` terminator (`sdk/typescript/src/client.ts` `makeTerminator`), which
patches outputs before `deleteSandbox`.

With no extract paths the whole workspace is captured with a diff; with explicit
paths each `/workspace` subtree becomes a narrowing, diff-bearing output.

### Callback-bridge egress allowlist (default-deny)

A conforming run reaches exactly one external endpoint: the instance
callback-bridge. `bridgeToEgressAllow` derives a `NetworkPolicy` with
`egress: "deny"` and the bridge `host:port` as the sole `allow` entry, reusing
the #219 egress model (`api/v1/types.go` `NetworkPolicy`,
`docs/networking.md`, `docs/threat-model.md`). Everything else is denied by
default. A bare `host:port`, or an `http`/`https`/`ws`/`wss` URL, is accepted;
the scheme is stripped and a default port inferred. An unparseable or portless
endpoint throws rather than silently widening egress. Operator-declared extra
allow entries (for example a rendezvous git remote or an inference proxy) are
merged with `withExtraEgress`, which keeps the default-deny posture and the
bridge entry first.

### Secrets at claim time (never in snapshots)

Secret VALUES never travel through the plugin. `secretsToClaimMounts` maps each
ref to a `SandboxSpec.Secrets` entry that carries only a Secret name and
key; the Mitos controller resolves the plaintext server-side at sandbox time
(`internal/controller/sandboxclaim_controller.go` `resolveSecrets`). This
enforces the secret-inheritance policy (`docs/fork-correctness.md` section 3):
sandbox-time secrets are injected at sandbox time and are never baked into a pool
snapshot, so a fork never inherits another run's credentials. Secret values are
never logged.

### Per-adapter install assertion

Adapter installs (the runtimes and CLIs an adapter needs) are baked into the
pool's snapshot at build time, NOT installed per run. `assertAdapterInstalls`
takes the required binaries plus a PATH probe of the running sandbox and returns
the missing set; `acquireWithAssertion` runs the probe right after the fork and,
on any missing binary, tears the sandbox down and raises an actionable error
("baked at pool build, not per run; rebuild the pool's template with these
adapters"). The assertion is sandbox-time and read-only; it never mutates the
sandbox.

## What is in this repo vs external follow-up

In this repo (`plugins/paperclip`):

- The pure contract mapping (`claim-mapping.ts`) and its tests.
- The claim-mode lifecycle orchestration (`claim-client.ts`) and its tests,
  driven through a `MitosClaimClient` seam (acquire, probe, patch-outputs,
  delete).
- The plugin wiring (`plugin.ts`): a `backend: server | claim` config, the
  claim-mode acquire and destroy handlers, and the manifest config schema.

The live cluster binding is the external follow-up. The plugin imports
`@paperclipai/plugin-sdk` (a `workspace:*` package that resolves only inside the
Paperclip monorepo), and the real `MitosClaimClient` over `@mitos/sdk`
`AgentRun` cluster mode is injected by the host via `setClaimClientFactory`. So
the contract mapping and lifecycle are proven here; the end-to-end binding lands
in the Paperclip monorepo.

### Workstreams B and C (external repos)

These live in the Paperclip and OpenClaw repos, not here:

- **B: paperclip-operator backend `microvm`.** A
  `spec.adapters.cloudSandbox.backend: jobs | microvm` knob, with knob mapping:
  `idleTimeoutMin` to claim idle reaping, `inferenceProxy` to a claim-time
  injection (an extra egress allow plus a secret ref), the image allow-list to a
  snapshot publishing policy, and the Redis rate limiter to a per-pool claim
  quota. The mitos-side primitives those knobs map onto already exist
  (`idleTimeout`, egress allow entries, claim-time secrets, pool quotas); the
  operator wiring is external.
- **C: shared operator core plus drivers.** Extract the common reconciler
  machinery into a versioned library and add the OpenClaw sandbox driver. Wholly
  external.

## Running the in-repo tests

The mapping and orchestration modules have no dependency on the external
`@paperclipai/plugin-sdk`, so they run on their own:

```bash
cd plugins/paperclip
# vitest config: plugins/paperclip/vitest.config.ts
vitest run
```
