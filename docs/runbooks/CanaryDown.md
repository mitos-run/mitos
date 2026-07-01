# Runbook: CanaryDown (and CanaryStale, CanarySandboxLeak)

## Signal

- **CanaryDown**: `mitos_canary_up == 0` sustained for 5m.
- **CanaryStale**: `time() - mitos_canary_last_success_timestamp_seconds > 900`
  (no green cycle for 15m) sustained for 5m.
- **CanarySandboxLeak**: `rate(mitos_canary_probe_total{phase="terminate",result="failure"}[15m]) > 0`
  sustained for 30m.

The synthetic canary (`cmd/mitos-canary`, deployed as the `mitos-canary`
Deployment) runs one cycle every `canary.interval`: it forks a sandbox from the
`canary.pool` template, execs `echo <nonce>`, verifies the nonce round-trips
with a zero exit code, then terminates the sandbox. It uses the exact
`HostedBackend` the SDKs use, so it fails the same way a customer would. This is
the closest signal to "a real user cannot fork right now".

`mitos_canary_up` is 1 only when the last full cycle passed create, exec, and
verify. It does NOT include terminate: a cleanup failure surfaces as
CanarySandboxLeak instead, because the user-facing fork path still worked.

## Liveness vs platform health

The canary's own `/healthz` (liveness) reports only whether the probe LOOP is
alive, never platform health. A real outage keeps the loop ticking and exporting
`mitos_canary_up=0`, and must NOT crash-loop the canary pod, or we lose the very
metrics these alerts read. So:

- Canary pod restarting or CrashLooping -> the canary itself is broken (bad api
  key, unreachable gateway at startup, config). Fix the canary.
- Canary pod healthy but `mitos_canary_up=0` -> the PLATFORM path is broken.
  Follow the diagnosis below; do not restart the canary.

## Likely causes (CanaryDown / CanaryStale)

Read `mitos_canary_probe_total{phase,result}` to see which phase fails:

- **phase=create failing**: the fork path is down. Warm pool starved, forkd not
  Ready, controller not placing claims, or the template cannot build. This is
  the same condition a user sees as `fork_unavailable`. Cross-check
  `WarmPoolStarved`, `ClaimsPendingSustained`, `ClaimErrorRateHigh`.
- **phase=exec failing**: fork works but the exec path is broken. Gateway ->
  forkd :9091 -> vsock -> guest agent. Check forkd HTTP health and the guest
  agent.
- **phase=verify failing** (create + exec returned but the nonce did not
  round-trip): the exec path is returning empty or wrong output, e.g. a proxy
  answering 200 without reaching the guest. Check the gateway routing and the
  guest agent exec stream.
- **Canary pod not Running / not Ready**: a bad or expired api key, or the
  gateway was unreachable. Check the pod and its logs (secret values are never
  logged; look for the phase and the redacted cause).

## Diagnosis

- `kubectl -n mitos get deploy,pod -l app.kubernetes.io/component=canary` and
  `kubectl -n mitos logs deploy/mitos-canary` (the per-cycle log names the failing
  phase and a redacted cause).
- Which phase: `sum by (phase,result) (rate(mitos_canary_probe_total[10m]))`.
- Latency by phase: `histogram_quantile(0.95, sum by (le,phase) (rate(mitos_canary_probe_duration_seconds_bucket[10m])))`.
- Confirm it is platform-wide, not canary-only: try the same path yourself with
  the SDK or `mitos sandbox create --pool <canary.pool>` against the gateway. If
  you also get `fork_unavailable`, it is the platform.
- Check the dependencies the canary rides: forkd readiness
  (`kubectl -n mitos get pods -l app.kubernetes.io/component=forkd`), the warm
  pool (`WarmPoolStarved`), the gateway, and the account/auth service (a create
  starts with auth).

## Remediation

- create failing: restore the warm pool and forkd (see `WarmPoolStarved` and
  `ClaimsPendingSustained` runbooks); confirm at least one KVM node is Ready.
- exec/verify failing: check forkd :9091 and the guest agent; restart the
  affected forkd if a node is wedged.
- Canary pod broken: rotate/repair the api key secret
  (`canary.apiKeySecretName`), confirm the gateway is reachable, redeploy.

## CanarySandboxLeak

The fork path works but terminate keeps failing, so every cycle leaks a
sandbox. Not user-facing, but it accumulates cost and can exhaust capacity.

- Check the terminate/DELETE path: `mitos_canary_probe_total{phase="terminate",result="failure"}`.
- List and reap orphaned canary sandboxes: `mitos sandbox ls` (or the hosted
  dashboard) and terminate leftovers. The GC's orphan sweep also reaps them; a
  concurrent `OrphanSweepSpike` is expected while this fires.

## Escalation

If create failures trace to snapshot builds or KVM with no obvious node cause,
escalate to the `internal/fork` / `internal/firecracker` on-call (snapshot build
path, security-sensitive).
