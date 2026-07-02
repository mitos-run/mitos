# Runbook: WarmPoolStarved

## Signal

`min by (pool) (mitos_pool_ready_snapshots) < 1` sustained for 10m.

`mitos_pool_ready_snapshots{pool}` is a controller gauge that mirrors
`SandboxPool.Status.ReadySnapshots`, set each pool reconcile. The desired-vs-ready
signal: a healthy pool holds this at its desired warm count. Near zero means the
warm pool is starved and the next claim into that pool cold-forks or pends. The
threshold (here `< 1`) is environment-tunable; set it to the pool's desired warm
count or a low-water mark.

## Likely causes

- Snapshot build is failing or stuck on the holder nodes (the `SnapshotsReady`
  pool condition is not True, or `TemplateBuilt` is `False/BuildFailed`).
- A snapshot that built but does not RESTORE: every dormant husk pod bound to
  it CrashLoopBackOffs forever. The pool's `TemplateHealthy` condition reports
  this as `False/RestoreFailing` (self-healing has detected it, but is still
  inside its backoff window) or `False/Rebuilding` (a rebuild is in flight).
  See `docs/husk-pods.md` (Self-healing template rebuild, #584).
- Husk pods are not at the desired replica count (the `HuskPodsReady` condition).
- Claims are draining the warm pool faster than it refills (demand spike).
- A pool with `spec.warm.min` below 2 has no headroom during a rebuild: a
  single claim can starve the pool until the next husk activates.
- No KVM-labeled node is available to hold the pool's snapshot.

## Diagnosis

- `kubectl mitos ls` and inspect the SandboxPool object: compare
  `status.readySnapshots` to the desired count.
- Pool `Ready` condition reason: `SnapshotsReady`, `HuskPodsReady` (healthy) vs a
  pending/failed reason. See `docs/conditions.md`.
- `TemplateBuilt` condition: `True/Built` vs `False/BuildFailed` (the build
  error itself, truncated to 512 characters).
- `TemplateHealthy` condition: `True/Healthy` vs `False/RestoreFailing` or
  `False/Rebuilding` (a restore-broken snapshot, self-healing already
  detected and is acting on it).
- `kubectl mitos ps` / `kubectl mitos top` to see whether holder nodes are
  present and have headroom.
- Metrics: `mitos_pool_ready_snapshots{pool}` (the gauge driving this alert),
  `mitos_claim_pending_total` (are claims pending as a result?),
  `mitos_claim_errors_total{reason="fork"}` (are snapshot builds erroring?).

## Remediation

- A restore-broken template usually self-heals: the controller detects two or
  more crashlooping dormant husks on the current digest and forces a rebuild
  with exponential backoff, no operator action needed. If it is stuck outside
  the backoff window, or an immediate rebuild is wanted, force one directly:

  ```
  kubectl -n mitos annotate sandboxpool <pool> mitos.run/force-rebuild="$(date +%s)" --overwrite
  ```

  Any new annotation value triggers exactly one rebuild, bypassing the
  backoff; this is now the documented recovery path and replaces the old hand
  recovery of deleting on-disk template artifacts and restarting forkd.
- Restore snapshot building: check forkd and the engine on the holder nodes
  (KVM health, snapshot artifacts).
- Raise the pool's desired warm count if demand structurally exceeds it.
- Add or recover KVM holder nodes so the pool can hold its snapshots.

## Escalation

If snapshot builds fail with no obvious node or KVM cause, escalate to the
`internal/fork` / `internal/firecracker` on-call (snapshot build path,
security-sensitive).
