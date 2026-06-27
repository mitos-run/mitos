# Fork SIGUSR2 opt-in (#467) and husk stale-digest invalidation (#461)

Date: 2026-06-27
Issues: #467 (SIGUSR2 broadcast silently kills non-handler workloads), #461 (stale snapshot digest on same-name pool rebuild).

Two independent fork-correctness bugs surfaced while bringing the Run-with-Mitos
serving workload (#460/#340) to a live node. Each lands with its tests in the same
commit (TDD), the relevant doc deltas, and a threat-model delta for #467.

## #467: SIGUSR2 broadcast silently kills tenant workloads that do not handle it

### Problem

The NotifyForked fork-correctness handshake broadcasts SIGUSR2 to every userspace
process after a fork (`guest/agent-rs/src/sys/signal.rs` `signal_userspace_at`),
to notify in-process runtimes to reset clock-derived deadlines and reseed
userspace PRNGs. SIGUSR2's POSIX default disposition is TERMINATE, so any process
that did NOT install a SIGUSR2 handler is KILLED on every fork. For short-lived
agent-exec workloads this is invisible (no long-lived app), but for a captured
serving app (Run with Mitos) it silently kills the server, producing a 502
"no route to guest port".

PR #468 (the #460 serving work) added a partial mitigation: a `WorkloadRegistry`
records the session id of a registered serving workload, and `select_targets`
skips that whole session. That protects a registered workload but leaves the root
hazard: any OTHER long-lived tenant process that does not handle SIGUSR2 is still
silently killed on every fork.

### Fix: opt-in by handler detection, layered under the existing session-exclusion

Signal a process ONLY when we can positively confirm it installed a SIGUSR2
handler. A process with no handler could never have acted on the reseed
notification anyway; it could only die. So restricting delivery to handler-having
processes loses nothing for fork-correctness and removes the silent-kill hazard.

Mechanism (`guest/agent-rs/src/sys/signal.rs`):

- New helper `process_catches_sigusr2(proc_path, pid) -> bool`: reads
  `<proc_path>/<pid>/status`, finds the `SigCgt:` line (the caught-signals
  bitmask the kernel exposes), parses the trailing hex as `u64`, and returns
  `mask & (1 << (libc::SIGUSR2 - 1)) != 0`. **Fail-safe:** any read or parse
  failure returns `false`. We never signal a process we cannot confirm handles
  SIGUSR2, so the broadcast can never kill a process by default.
- `select_targets` gains a final gate: after the existing exclusions (PID 1,
  self, excluded workload session), include a pid only if
  `process_catches_sigusr2` is true.

### Why keep the session-exclusion (refinement to the chosen approach)

Handler-detection does NOT cleanly subsume the #460 session-exclusion. Real
servers overload SIGUSR2 for their own control plane: nginx uses SIGUSR2 for
binary upgrade, and other daemons use it for reload. Such a captured workload DOES
have a SIGUSR2 handler, so pure handler-detection would SIGNAL it and trigger its
upgrade/reload path on every fork. The session-exclusion leaves the captured app
entirely alone regardless of whether it handles the signal. So the layering is:

1. Skip PID 1, self, and any registered-workload session (protect the captured
   app, handler or not). Unchanged from #460.
2. Among everyone else, signal only confirmed SIGUSR2 handlers (protect
   unregistered non-handler tenant processes from the silent kill). New in #467.

`SigCgt` semantics: `/proc/<pid>/status` exposes a 64-bit hex mask where bit
`(signo - 1)` is set when the process has a handler installed (caught) for
`signo`. SIGUSR2 is 12 on Linux x86_64 and aarch64 (bit 11); the code derives the
bit from `libc::SIGUSR2` rather than hardcoding. A process that IGNORES SIGUSR2
(`SIG_IGN`) is not in `SigCgt` and is skipped, which is correct: signaling it is
pointless and skipping it is safe.

### Tests (guest/agent-rs, host-safe)

- `process_catches_sigusr2` parsing: a `SigCgt` mask with the SIGUSR2 bit set
  returns true; without it returns false; a missing `status` file returns false;
  a malformed line returns false.
- Update `excludes_pids_in_an_excluded_session`: write a `status` file with the
  SIGUSR2 bit set for the unrelated pid so it stays a target, and add a pid whose
  `SigCgt` lacks the bit and assert it is NOT selected.
- New `targets_only_processes_that_catch_sigusr2`: synthetic /proc with pid A
  (handler) and pid B (no handler) selects only A.
- Update `sigusr2_delivered_to_child_via_synthetic_proc`: the child installs a
  SIGUSR2 handler, so the synthetic /proc child dir must now include a `status`
  file with the SIGUSR2 `SigCgt` bit set, else fail-safe-skip would (correctly)
  not signal it. The updated test proves we DO signal a confirmed handler.

No new `unsafe`: `process_catches_sigusr2` is pure file parsing.

### Doc and threat-model deltas

- `docs/fork-correctness.md`: update the SIGUSR2 signal row (legend table and the
  agent-as-init row) to state the broadcast now delivers only to confirmed
  SIGUSR2 handlers, with the registered-workload session still fully excluded.
- `docs/threat-model.md`: extend the serving-workload note (the per-fork SIGUSR2
  reset paragraph) to record that the broadcast no longer default-terminates
  non-handler processes, closing an availability/self-DoS hazard on the fork path.

## #461: stale snapshot digest on same-name pool rebuild

### Problem (root cause confirmed)

Template snapshots are stored on disk by template id
(`internal/fork/engine.go` `templateDir` = `<dataDir>/templates/<templateID>/`),
and `poolTemplateID` keys the template by POOL NAME
(`internal/controller/pool_template.go:46`). So rebuilding a pool under the same
name OVERWRITES the snapshot mem in place and produces a new content-addressed
manifest digest.

New husk pods always get the fresh per-node digest: the reconcile reads
`NodeRegistry.SnapshotHolders(templateID)` (refreshed from forkd's `GetCapacity`,
`forkd_discovery.go:139`) and sets `podOpts.ExpectedDigest = pick.Digest`
(`huskpod.go:1148`). But EXISTING warm husk pods keep the OLD `ExpectedDigest`
baked into their stub args / manifest mount. When the mem is overwritten under
them, the husk re-hashes the NEW mem against the OLD manifest and fails prepare:

```
file "mem" failed integrity verification: chunk 0 recorded <OLD> does not match
on-disk <NEW>
```

The husk reconcile only scales the warm pool up (create) and down (delete extras)
by COUNT. It never reaps a dormant pod merely because its baked digest no longer
matches the current snapshot, so stale warm husks CrashLoopBackOff forever. A
fresh pool NAME loads fine (no prior recorded digest) which is the bad-DX
workaround today. PR #462 (fsync before recording the digest) did not fix this:
the divergence is a stale baked value, not a flush/durability gap.

### Fix: invalidate (reap) dormant husk pods whose stamped digest is stale

Make a warm husk pod carry the digest it was built against, and let the reconcile
reap any DORMANT pod whose stamped digest no longer matches its node's current
reported digest. The existing deficit logic then recreates it with the fresh
digest, which verifies against the new mem.

Mechanism (`internal/controller/huskpod.go`):

- Stamp each husk pod at creation with a `mitos.run/template-digest=<digest>`
  label (the pod already records its pinned node via `SnapshotNodes` -> node
  affinity / `spec.nodeName`). Empty digest (pre-digest fallback path) is stamped
  as empty and is never considered stale.
- In the husk reconcile, after computing the per-node `SnapshotHolders` (the fresh
  digests), scan the DORMANT (unclaimed, not active) owned husk pods. A pod is
  STALE when its node has a known current digest for the templateID and that
  digest differs from the pod's stamped label. Delete stale dormant pods (never a
  claimed or active pod, which holds a tenant's running VM). The scale-up branch
  then refills the deficit with fresh-digest pods.
- Reap before the count-based scale logic so a stale pod is not counted as a
  healthy warm slot.

This is purely controller-side and does NOT trigger a rebuild on content change
(that is #475, explicitly out of scope here). It only ensures that ONCE a rebuild
has happened, warm husks converge to the current snapshot instead of crash-looping.

### Tests (envtest)

- A dormant husk pod stamped with an old digest, on a node whose registry digest
  is now new, is deleted by the reconcile; a fresh-digest pod is created.
- A dormant husk pod whose stamped digest MATCHES the node's current digest is NOT
  deleted (no churn in steady state).
- A CLAIMED/active husk pod with a stale digest is NOT deleted (never reap a
  running tenant VM).
- An empty-digest fallback pod (no node digest known) is NOT treated as stale.

### Verification boundary (honest)

The issue's acceptance is a live KVM run: snapshot a memory-allocating /
running-workload pool, change it, rebuild under the same name, and confirm the
warm husks converge and fork. The envtest above proves the controller reap/refill
logic; the final live-cluster confirmation is the maintainer's, since KVM cannot
run from CI here. This boundary is stated on the PR and the issue.

## Out of scope

- #475 (a workload-command change does not re-trigger a rebuild). Paired, separate.
- Content-addressed template identity (folding a content hash into `templateID`).
  Considered and declined for this change: larger blast radius across naming, GC,
  and distribution; the reap-and-refill fix is sufficient for #461's acceptance.
