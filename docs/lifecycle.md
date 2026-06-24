# Sandbox lifecycle: timeouts, pause/resume, idle, expiry

This is the reference for how long a sandbox lives, what keeps it alive, how to
adjust its TTL while it runs, and how to pause and resume it. It reconciles the
controller-side reaping (ROADMAP section 2, `docs/failure-gc.md`) with the
sandbox HTTP API surface (issue #218) that the SDKs expose.

The three controls:

- `set_timeout`: adjust a RUNNING sandbox's TTL (live, not just at creation).
- pause / resume: snapshot full state (memory + filesystem) and stop the clock,
  then restore.
- work-aware idle timeout: idle is measured against ACTUAL activity, including a
  running background process, not just inbound API interaction.

## Timeouts and TTL

Two creation-time bounds, set on the Sandbox spec (k8s mode) or implied by
the standalone sandbox-server defaults:

| Bound | Field | Meaning | Default |
| --- | --- | --- | --- |
| maxLifetime | `spec.lifetime.ttl` | Hard wall-clock cap from start. The sandbox is reaped at `startedAt + ttl` regardless of activity. | unset (no cap) |
| idleTimeout | `spec.lifetime.idleTimeout` | Reap after this much time with no ACTUAL activity (see below). | unset (no idle limit) |

There is no implicit default for either: a zero or unset value means "no limit".
Operators set these per sandbox or per pool. The standalone sandbox-server does
not reap on its own; reaping is a controller (k8s) behavior, and the live
`set_timeout` deadline below is the standalone path's TTL control.

maxLifetime does not depend on a reachable forkd: it is pure wall-clock from
`startedAt`. idleTimeout and the live deadline are evaluated from the work-aware
activity signal forkd reports through `ListSandboxes`.

### Live `set_timeout`

`set_timeout(timeout_seconds)` adjusts a RUNNING sandbox's TTL to
`now + timeout_seconds`. It is exposed as:

- `POST /v1/set_timeout` on forkd and the standalone sandbox-server, body
  `{"sandbox": "<id>", "timeout_seconds": <n>}`, returning the new
  `deadline_unix`.
- `sandbox.set_timeout(n)` in the Python SDK (sync `Sandbox` and `DirectSandbox`,
  async `AsyncSandbox`) and `sandbox.setTimeout(n)` in the TypeScript SDK.

The live deadline takes authority over the idle clock: while a live deadline is
set and in the future, the sandbox is not idle-reaped (the caller has taken
explicit control of the TTL). A live deadline in the past reaps the sandbox with
the `TimeoutExpired` reason. This is the seam the E2B compat shim (#206) maps its
`setTimeout` onto.

Ceiling and rejection (issue #216): a requested timeout over the server ceiling
(`--max-exec-timeout-seconds`, default 86400 s = 24 h) is REJECTED with the typed
`timeout_too_large` error, never silently clamped. The deadline you set is the
deadline you get, or you get a clear rejection that names the ceiling.

## Work-aware idle timeout

Idle is measured against ACTUAL activity, not just inbound API interaction. A
sandbox is NOT idle when any of these hold:

- a streaming exec, run_code, or PTY session is OPEN (a live background job), or
- the sandbox is paused (its clock is stopped while held), or
- the most recent inbound exec or file interaction is within the idle window.

Only when none of these hold, and the time since the later of last-activity and
start exceeds `idleTimeout`, is the sandbox reaped with the `IdleTimeout` reason.

This is the difference that matters for unattended jobs: a long-running
background process with no inbound interaction is NOT killed mid-run. forkd
surfaces the open-stream count (`active_streams`) and the paused flag through
`ListSandboxes`; the controller's idle decision (`idleExpired`) treats a non-zero
stream count or a paused sandbox as busy. The decision function is unit-tested on
the mock (`internal/controller/idle_decision_test.go`,
`TestClaimIdleTimeoutNotReapedWithBackgroundJob`).

Default: there is no implicit idle window; idle reaping is off unless
`idleTimeout` is set. When it is set, the work-aware rule above governs.

## Pause and resume

Pause snapshots the sandbox's FULL state (guest memory + filesystem) and pauses
the VM; resume restores it exactly. A paused sandbox is held, not reaped: its
idle clock is stopped and the billing meter stops (coordinate with the usage
pipeline, #208). Repeated pause/resume cycles preserve both memory and
filesystem state.

Exposed as:

- `POST /v1/pause` and `POST /v1/resume` on forkd and the standalone
  sandbox-server, body `{"sandbox": "<id>"}`.
- `sandbox.pause()` / `sandbox.resume()` in the Python SDK (sync and async) and
  `sandbox.pause()` / `sandbox.resume()` in the TypeScript SDK.

Substrate: the snapshot/fork engine (`internal/fork`, `Engine.Pause` /
`Engine.Resume`) drives a Firecracker Full snapshot of the running VM paired with
the copy-on-write rootfs that already holds the filesystem, so both survive every
cycle. forkd wires the engine pause/resume into the HTTP endpoints
(`SandboxAPI.SetEnginePauser`); the standalone server and unit tests record the
held state only (no VM behind them).

### Validation status

- The pause/resume API surface, the held-state bookkeeping, the work-aware idle
  decision, and the live `set_timeout` deadline are unit-tested on the mock
  engine and in controller envtests (no KVM).
- The REAL memory + filesystem preservation across N repeated pause/resume
  cycles needs KVM (the mock cannot snapshot real memory). It is asserted by the
  GATED test `TestEnginePauseResumePreservesStateKVM`
  (`internal/fork/engine_pause_kvm_test.go`), which boots a real Firecracker VM,
  writes a marker file and starts a long-running process, runs N pause/resume
  cycles, and asserts the file content and the same live PID survive every cycle.
  It skips cleanly when `/dev/kvm` or the asset env vars are absent, so it is
  never a fake pass: it only asserts when it can really boot a VM. The KVM CI
  workflow (`.github/workflows/kvm-test.yaml`) provides the runner and assets.

This directly targets the documented competitor papercuts: the E2B
repeated-cycle filesystem bug (state not persisting after multiple pause/resume)
and the Daytona interaction-only idle timer (background jobs killed mid-run).

## Behavior on expiry

When a sandbox crosses its bound it is TERMINATED, not paused: the backing VM is
reaped and the claim reaches the terminal `Terminated` phase with a condition
carrying the reason (`MaxLifetimeExceeded`, `IdleTimeout`, or `TimeoutExpired`).
A subsequent call against a reaped sandbox returns the typed `idle_timeout` error
(`docs/api/errors.md`), whose remediation points at creating a fresh sandbox or
calling `set_timeout` earlier to keep it alive. Pause is the explicit way to hold
a sandbox without terminating it; expiry never auto-pauses.
