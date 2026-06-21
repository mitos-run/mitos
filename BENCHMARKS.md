# Benchmarks

Every latency number this project publishes must come from a benchmark anyone
can rerun. This file documents the methodology behind the reproducible harness
and records where the current (shared-CI-class) numbers come from. It is
deliberately honest about what is measured and what is still open.

## What is measured today

The harness is `cmd/bench`. It imports `internal/fork` and drives the real
KVM-backed engine in-process (no forkd, no gRPC, no HTTP API in the path), so
the timing reflects the fork + vsock + guest-agent data path and nothing else.
The percentile statistics are computed by `internal/benchstat` (count, min, p50,
p90, p99, max, mean; nearest-rank percentiles).

Two modes:

- **`fork-exec`** (`fork_to_first_exec`): measures the wall time from the start
  of a fork to the first successful exec result returned through the guest
  agent. Each iteration forks a fresh sandbox from the template snapshot,
  connects to the fork's Firecracker vsock UDS, execs a trivial command, and
  terminates the sandbox. The clock stops the instant the first exec result is
  in; teardown (SIGKILL of Firecracker, process wait, and removal of the
  sandbox/jailer chroot) runs after the timer has stopped and is excluded from
  the measured duration. This is the cold-claim-shaped number: snapshot restore
  plus the time for the guest agent to service the first exec.
- **`exec-rt`** (`exec_round_trip`): forks one sandbox once, warms the
  connection and the guest exec path, then measures a stream of trivial exec
  round-trips against the already-warm agent. This isolates the warm exec
  hot-path (vsock round-trip + `/bin/sh -c` spawn in the guest) from the
  one-time restore cost.

Warmup iterations are discarded; they pay the page-cache and snapshot-load costs
that should not skew the measured samples.

## Hardware and configuration (CI run)

- **Runner:** GitHub Actions `ubuntu-latest` (a shared, oversubscribed runner,
  frequently itself nested-virt). This is NOT bare metal.
- **VMM:** Firecracker v1.15.0.
- **Kernel:** the Firecracker CI kernel (`vmlinux-*`) pulled from
  `spec.ccfc.min` for the v1.15 CI series.
- **Rootfs:** a minimal ext4 image with the project's guest agent as `/init`
  plus a static busybox (`/bin/sh`, `/bin/true`, and the small tool set the
  agent's exec path needs).
- **Iterations:** 30 measured, 5 warmup, per mode (modest on purpose; the runner
  is noisy).
- **VM size:** 1 vCPU, 256 MiB.

## Results

The numbers are produced by the `KVM Integration Test` workflow on every run of
the bench phase and are **SHARED-CI-CLASS**: noisy, oversubscribed, and not
representative of bare metal. They exist to prove the harness runs end to end and
to give a reproducible distribution that is regenerated every run, not to be
quoted as the product's latency.

> Populated from the CI artifact. The latency tables for `fork_to_first_exec`
> and `exec_round_trip` are printed to the step log AND appended to the run's
> job summary by the bench phase, and the raw JSON is uploaded as the
> `bench-results` workflow artifact. See the most recent `KVM Integration Test`
> workflow run summary for the current shared-CI-class distribution. (We do not
> paste numbers here: this file is committed before the post-merge CI run that
> produces them, and a hand-copied number would be exactly the kind of
> unverifiable claim this harness exists to eliminate.)

To regenerate the numbers on your own hardware, see [`bench/README.md`](bench/README.md).

## CoW density datapoint

A separate datapoint measures the memory density of forking, not latency: when N
sandboxes are forked from ONE template, every fork restores the SAME snapshot
with `MAP_PRIVATE`, so they map the same shared page set. The honest physical
footprint counts that shared template region ONCE; the marginal cost of an
additional fork is its unique (private-dirty) set. The naive accounting counts
the shared region once per fork and overstates the footprint.

This is produced by `cmd/bench --mode metering`, which forks N (default 4) real
sandboxes from one template, lets them settle, and reads the engine's CoW-aware
metering `Report` (memory from `/proc/<pid>/smaps_rollup`, disk from stat). The
**metering CI phase** in the `KVM Integration Test` workflow runs it with
`--forks 4`, asserts the CoW-aware total is below the naive total, that
`CoWSavings` is positive and at least one shared-template set (the shared region
deduplicated across all forks), and that each fork's unique set is smaller than
the once-paid shared set. It then publishes the byte counts to the run's job
summary.

The reported metrics are:

- `UsedCoWAware`: sum of per-fork unique plus each template's shared set counted
  once. The honest resident footprint.
- `UsedNaive`: sum of per-fork unique plus every fork's shared set (the shared
  region double-counted), for comparison.
- `CoWSavings`: `UsedNaive - UsedCoWAware`, the bytes the CoW model reveals are
  not actually consumed per fork.
- per-fork `MemoryUnique`: the marginal physical cost of one additional fork.

These are **SHARED-CI-CLASS** (noisy `ubuntu-latest`), reproducible per run, and
NOT bare-metal figures.

> Populated from the CI run. The byte counts (`UsedCoWAware` vs `UsedNaive`,
> `CoWSavings`, per-fork unique, shared-once) are printed to the metering CI
> phase step log AND appended to the run's job summary as a table, and the raw
> report JSON is uploaded as the `metering-report` workflow artifact. See the
> most recent `KVM Integration Test` run summary for the current shared-CI-class
> density numbers. As with the latency tables, we do not paste numbers here: a
> hand-copied number would be exactly the kind of unverifiable claim this
> harness exists to eliminate. The aggregation rules and what is exact vs
> approximate are documented in [`docs/metering.md`](docs/metering.md).

## Husk-stub activation latency datapoint

A separate datapoint measures the claim-time cost of the husk-pods prepare/
activate split (issue #18; see [`docs/husk-pods.md`](docs/husk-pods.md)). In that
model the Firecracker VMM is pre-started DORMANT before a claim arrives
(prepare), so the only cost paid at claim time is activating it: loading the
template snapshot in place, resuming, and waiting for the guest agent to answer
over vsock. This datapoint is that activation latency, NOT a full VMM spawn.

It is produced by the **husk-stub CI phase** in the `KVM Integration Test`
workflow. The phase reuses the bench template snapshot and, for each iteration,
starts a fresh dormant `cmd/husk-stub` (prepare), runs `husk-stub --activate`
to activate the snapshot in place, asserts the `ActivateResult` is `OK`, and on
the first iteration execs a real command through the guest agent over the
returned vsock path. The gate is activate OK AND a working exec. It publishes
nearest-rank P50/P99 of the stub-measured `LatencyMs` (load-start to
guest-ready) to the run's job summary.

These are **SHARED-CI-CLASS** (noisy `ubuntu-latest`), reproducible per run, and
NOT bare-metal figures. The **<= 10ms warm activation figure is the bare-metal
reference-node TARGET (#18/#15), not a shared-CI claim**: this phase does not
assert it and the shared-CI activation latency must not be quoted as achieving
it.

> Populated from the CI run. The min/P50/P99/max activation latency table is
> appended to the run's job summary, and the per-iteration result JSON plus the
> raw latencies are uploaded as the `husk-stub-activation` workflow artifact. See
> the most recent `KVM Integration Test` run summary for the current shared-CI-
> class activation latency. As with the other tables we do not paste numbers
> here: a hand-copied number would be exactly the kind of unverifiable claim this
> harness exists to eliminate.

## Bare-metal reference node (#16)

The sections above are SHARED-CI-CLASS: noisy `ubuntu-latest`, frequently
nested-virt, regenerated per run. This section records the first reference-node
numbers measured on the #16 bare-metal reference hardware. These are the only
bare-metal numbers the project publishes as its own, and each one traces to a
reproducible source in `bench/`.

### Reference node

- Hardware: Hetzner dedicated, Intel Core i7-6700 (4c / 8t, 8 logical CPU),
  64 GiB RAM.
- OS / cluster: Talos Linux, kernel 6.18.33-talos; `/var/lib/mitos` on xfs.
- Path: the default husk path (unprivileged pod, `/dev/kvm` via the device
  plugin).
- Template: `ghcr.io/paperclipinc/sandbox-base-python:3.12-slim`.
- VMM: Firecracker v1.15.0, verify-at-Prepare (the integrity gate is paid in the
  dormant Prepare phase, so the activate runs at engine speed).

### Measured (this node)

| metric | value | what it measures | source |
| --- | --- | --- | --- |
| warm-claim activate latency | P50 ~27 ms (N=11: min 21.45, P50 26.53, P95 46.66, max 46.66 ms) | husk-stub-reported snapshot load + fork-correctness handshake + guest-ready, parsed from the claim's Ready condition message ("activated ... in X ms") | `bench/husk-activate-latency.sh`, results in `bench/results/2026-06-13-bare-metal-husk.md` |
| snapshot restore (`/snapshot/load`) | ~6-16 ms | the Firecracker engine restore step alone | `bench/results/2026-06-13-bare-metal-husk.md` (forkd / husk-stub logs) |
| fork-to-first-exec | P50 ~104 ms (N=50 quiesced node: min 77.9, P50 104.0, P90 109.8, P99 112.1 ms; matches the N=20 co-located P50 103.9) | `cmd/bench` fork-exec: fork from snapshot, restore, first exec result; ~16 ms is `/snapshot/load`, the rest is lazy page-fault-in + guest agent (the tail #167 targets). Validated contention-free: a quiesced reference node gives the same P50, so co-location does not inflate it (#16). | `bench/fork-exec-job.yaml`, results in `bench/results/2026-06-19-bare-metal-fork-exec.md` |
| marginal memory per forked sandbox | ~3 MiB | per-VM unique (private-dirty) cost via CoW page sharing; the shared snapshot page set is counted once across cgroup v2 memcgs (per-VM dirty ~5 MiB, not overstated) | husk-probe CI proof; `docs/metering.md` for the accounting rules |

### Honest scope of the activate number

The ~27 ms is the engine activate the controller records, NOT the end-to-end
claim->Ready wall clock. On this node that wall clock is ~0.5-1.8 s and is
reconcile-bound: the Kubernetes control-loop round-trip (watch + queue + status
poll) plus warm-pool refill, NOT the engine. We report the variance and do not
present the activate as the wall-clock claim latency.

The bare-metal engine fork->first-exec was NOT re-measured this session. The
`cmd/bench` `fork-exec` harness number (shared-CI-class, see "Results" above)
remains the cited fork->first-exec figure; no bare-metal fork->first-exec number
is stated.

### How to reproduce

```sh
bench/husk-activate-latency.sh <kubeconfig> <pool> [namespace] [iterations]
```

The script creates N sequential `SandboxClaim`s against a warm pool, waits for
Ready, parses the activate latency out of the Ready condition message, releases
each claim between iterations, and prints min / P50 / P95 / max plus the raw
samples. The full node spec, sample set, restore samples, CoW basis, and cluster
setup are in
[`bench/results/2026-06-13-bare-metal-husk.md`](bench/results/2026-06-13-bare-metal-husk.md).

The **<= 10ms warm activation figure remains the bare-metal TARGET (#18/#15)**:
the activate restore step reaches sub-10 ms on this node, but the full activate
(restore + handshake + guest-ready) measures ~27 ms P50 here, so the <= 10ms
claim->first-exec target is not yet met end to end and stays OPEN.

## Raw-forkd vs pod-native: claim to first exec

This section synthesizes the two existing shared-CI datapoints above into the
claim-to-first-exec comparison between the raw-forkd execution model and the
husk-pods pod-native model (issue #18).

### Two hot-path definitions

**Raw-forkd claim-to-first-exec.** When a claim arrives in raw-forkd mode, forkd
spawns a fresh Firecracker process, loads the template snapshot, and waits for the
guest agent to answer. The full cost is paid at claim time: VMM process spawn +
snapshot load + guest-ready. This is exactly what the bench harness measures as
`fork_to_first_exec` (see the "Results" section above; the `fork-exec` mode of
`cmd/bench`). The shared-CI P50/P99 are produced by the bench phase of the `KVM
Integration Test` workflow on every run.

**Pod-native (husk pods) claim-to-first-exec.** In husk-pods mode a warm pool of
pre-scheduled pods each hold a DORMANT Firecracker VMM (the "prepare" side: VMM
process already started, no snapshot loaded). When a claim arrives, the controller
activates one of those dormant husk pods over the mTLS control channel: the stub
loads the template snapshot in place, resumes the VM, and waits for the guest agent
to answer. The claim-time cost is therefore: in-place snapshot load + resume +
guest-ready (the activation), plus the controller mTLS control round-trip. The VMM
process spawn is NOT on the claim hot path; it happened at warm-pool-fill time,
before the claim arrived. The shared-CI P50/P99 for the activation alone are
produced by the husk-stub CI phase of the `KVM Integration Test` workflow on every
run (see the "Husk-stub activation latency datapoint" section above).

### Comparison

Both datapoints are SHARED-CI-CLASS (noisy `ubuntu-latest`, reproducible per run,
NOT bare metal). Rather than paste numbers here (which would be unverifiable between
runs), the comparison references the two CI phases by role:

| path | claim-time cost | shared-CI P50/P99 source |
| ---- | --------------- | ------------------------ |
| raw-forkd | VMM spawn + snapshot load + guest-ready (`fork_to_first_exec`) | bench phase: `fork-exec` mode, `KVM Integration Test` summary and `bench-results` artifact |
| pod-native (husk) | snapshot load + resume + guest-ready + mTLS round-trip (activation only) | husk-stub phase: `Husk-stub in-place activation latency`, `KVM Integration Test` summary and `husk-stub-activation` artifact |

See the most recent `KVM Integration Test` run summary for the current shared-CI-class
values for both paths. The bench phase and the husk-stub activation phase each print
their own P50/P99 table to the same summary, so they can be read side by side.

### The design win: VMM spawn is off the claim hot path

The key property of the husk-pods model is that the Firecracker process spawn
(and pod scheduling, admission, cgroup creation) is amortized to warm-pool-fill
time and is NOT paid when a claim arrives. The claim hot path is just activation:
snapshot load + resume + guest-ready, which excludes the VMM spawn cost that
raw-forkd's `fork_to_first_exec` includes.

This means pod-native is competitive with, or faster than, raw-forkd on the
claim hot path: if the husk activation P50 is below the raw fork P50 (as the
shared-CI runs show, because the FC spawn is pre-paid), pod-native is NOT 2-3x
slower. Pod-native is therefore NOT slower than raw-forkd on the claim path by
the threshold that would make it unacceptable; it inverts the concern.

The one component that pod-native adds versus raw-forkd is the controller mTLS
control round-trip (the activate RPC from the controller to the husk pod stub
over the network control channel). That round-trip is real and honest: it is a
network call that raw-forkd's local gRPC-to-daemon path does not pay. The shared-CI
activation latency includes it (the `LatencyMs` the stub reports is load-start to
guest-ready, and the mTLS handshake precedes it), but the two paths' overall P50s
are still comparable because the dominant cost eliminated (the FC spawn) is larger
than the mTLS overhead added. The mTLS round-trip is the one component that is
slower than the equivalent step in raw-forkd.

### Warm-pool-fill cost (honest, off the hot path)

The cost that raw-forkd pays at claim time and husk pods pay off the hot path:

- **Pod scheduling and admission:** the Kubernetes scheduler places the husk pod,
  the admission webhook runs, and the kubelet starts the container. This is on the
  order of seconds on a live cluster (Kubernetes scheduler round-trip + kubelet
  startup). It is paid when the warm pool is initially filled or when a pod is
  replaced after eviction or drain.
- **VMM Prepare:** `cmd/husk-stub` starts a dormant Firecracker VMM (process +
  API socket, no snapshot). This is also paid at warm-pool-fill time, not at claim
  time. Its cost is similar to the VMM spawn component of raw-forkd's
  `fork_to_first_exec`.

Both costs are paid per warm slot and are therefore on the order of seconds
(scheduling) per slot. The warm pool must be sized ahead of demand, or autoscaled
from the `mitos_claim_pending_total` metric (the pending-claims signal, issue
#17). An undersized warm pool degrades to cold-start: scheduling a fresh husk pod
at claim time, which is the slow path (seconds, not the warm-pool activation
latency). The warm pool is the capacity lever that keeps the claim hot path fast.

### Bare-metal target (OPEN, #16 / #18)

On a noisy shared GitHub runner, both the bench phase and the husk-stub activation
phase measure SHARED-CI-CLASS numbers that include nested-virt penalty and runner
oversubscription. On a bare-metal KVM node the activation latency is expected to
be far lower.

**<= 10ms warm-pool claim-to-first-exec on bare metal** is the directive TARGET
(issue #18 constraint, issue #16 reference node). This is NOT a shared-CI claim.
It has not been measured; the reference hardware (Hetzner + Talos, issue #16) does
not yet exist as a pinned CI runner. This target remains OPEN and will be measured
when the pinned reference node is provisioned. Until then, the bare-metal
activation latency is not stated.

## Facade vs upstream reference: resume latency

This section frames the resume-latency comparison between our `agents.x-k8s.io`
facade (issue #19) and the upstream reference controller, for the upstream
pause/resume contract (the Sandbox `spec.replicas` 0<->1 toggle; upstream v0.4.6
has no stateful hibernate field, so pause/resume IS that toggle). The harness is
`bench/facade/` (see [`bench/facade/README.md`](bench/facade/README.md)).

### The two resume paths

| system | resume = replicas 0 -> 1 does | dominant cost |
| --- | --- | --- |
| our facade | RE-ACTIVATES a dormant warm husk pod: re-create the bridged `SandboxClaim`, the warm pool hands back a pre-prepared husk (snapshot load + resume + guest-ready) | the husk activation: ~42ms P50 SHARED-CI (#66; the "Husk-stub activation latency datapoint" section above), NOT a fresh pod |
| upstream reference (v0.4.6) | COLD-CREATES a pod: delete the pod on 0, create a fresh one on 1 | pod schedule + admission + image + container start + app boot, on the order of seconds |

The facade resume re-activates a warm dormant VM; the upstream resume cold-creates
a pod. The **order-of-magnitude resume advantage is the DESIGN claim**: a
warm-pool re-activation (the ~42ms husk activation datapoint, #66) is
fundamentally cheaper than a cold pod create (seconds). We do not paste a single
head-to-head number here.

### What the harness measures, and what is a target

- On a **shared-CI / kind** cluster the husk VMM does not boot (the #18
  boundary), so the **in-VM resume tail is NOT measurable** and a naive
  head-to-head would falsely flatter the upstream side (its container does boot
  on kind, ours cannot). `bench/facade/` therefore measures only the
  OBJECT-LEVEL resume latency on kind (the facade re-creating the bridged claim),
  and the `facade-conformance` kind job asserts the object-level resume
  (replicas 1->0->1 releases then re-creates the claim).
- The **same-cluster in-VM head-to-head number** (our warm husk re-activation vs
  the upstream cold pod create, both timed to a serving endpoint) is a
  **bare-metal-reference-node TARGET (#16)**. It cannot be measured on kind; it
  is reproducible from `bench/facade/` on a KVM-capable kubelet with the upstream
  controller deployed alongside, exactly as the README documents.

Every number is sourced from a datapoint (the ~42ms husk activation, #66, from
the husk-stub CI phase) or the `bench/facade/` harness, or is marked a target
(#16). The representative upstream cold-start figure ("on the order of seconds"
for pod schedule + image + container start + app boot) is the documented,
clearly-labeled cold-start range, not an invented measurement; the precise
same-hardware figure is what the bare-metal harness run produces.

## Controller-path harness (claim, sustained, pool-rebuild)

Issue #15 items 1-3 are the controller + pool path the in-process `cmd/bench`
harness does NOT cover. They are now RUNNABLE harnesses under `bench/` that drive
a REAL cluster over a kubeconfig. They are **method-only here**: the harness
ships in-repo, but the NUMBERS are produced by a maintainer running them on a
cluster on documented hardware and recorded in `bench/results/`. None is faked.
On a host with no reachable cluster (a darwin laptop) each fails at client
construction with a clear message and prints no number.

- **Claim to first-exec end to end through the controller** (#15 item 1):
  `bench/claim-first-exec-latency.sh <kubeconfig> <pool> [ns] [iters]`, the
  `claim-exec` mode of the `bench/claim` Go harness. For each sequential claim it
  creates a `SandboxClaim`, waits for the controller to drive it Ready, and runs
  the FIRST exec over the sandbox HTTP API (the same endpoint + per-sandbox
  bearer token kubectl-sandbox and the SDK use), measuring claim-create ->
  first-exec P50/P90/P99. This is the full controller + scheduler + pool path, NOT
  the engine data path `cmd/bench` measures. **Status: harness runnable on a
  cluster; numbers OPEN, pending the #16 reference node.**
- **Sustained claims/sec and density curve** (#15 item 2):
  `bench/sustained-claims-throughput.sh <kubeconfig> <pool> [ns] [rate] [duration] [max_concurrent]`,
  the `sustained` mode. It arrives claims at a target rate for a window and
  records achieved claims/sec, peak concurrency, and per-node density (sweep the
  rate to trace the density curve and find where achieved/sec stops tracking the
  target). Aggregation is the unit-tested `internal/benchstat.AggregateThroughput`.
  **Status: harness runnable on a cluster; numbers OPEN.**
- **Pool-rebuild propagation** (#15 item 3):
  `bench/pool-rebuild-propagation.sh <kubeconfig> <pool> [ns]`, the `pool-rebuild`
  mode. It triggers a pool rebuild and times from the spec change to all-nodes
  reporting the snapshot ready (`ReadySnapshots == TotalSnapshots`), the
  snapshot-distribution propagation latency. Meaningful on a multi-node cluster;
  on a single node it measures that node and says so. **Status: harness runnable
  on a multi-node cluster; numbers OPEN.**

## Competitor comparison (scaffold + methodology)

Issue #15 item 5 asks for a head-to-head against E2B (self-hosted), Daytona
(OSS), and Agent Sandbox + Kata, on identical hardware, reproducible from
in-repo scripts. Running a competitor requires standing up that competitor's own
stack, which is out of this repo's control, so what ships in-repo is a
**scaffold + methodology**: `bench/competitors/` with a neutral driver
(`run-comparison.sh`) that measures every system by the SAME create-sandbox ->
first-exec method, a reference adapter (`adapters/mitos.sh`) wired to this repo's
own harness, and placeholder adapters for each competitor that a reproducer fills
in (they exit non-zero until then, so a run can never emit a fabricated competitor
number). The honesty rule is explicit in `bench/competitors/README.md`: we
publish a mitos number only from our own harness on documented hardware, and any
competitor figure not measured here on the same hardware is labeled
**vendor-published** (with a citation), NOT our measurement. **Status: scaffold +
methodology in-repo; head-to-head numbers OPEN, pending the #16 reference node and
a reproducer standing up each competitor.**

## Items still needing the pinned bare-metal reference node (#16)

Every controller-path number above and the competitor head-to-head need the
pinned #16 bare-metal reference node before they can be published as measured.
Concretely OPEN pending #16:

- claim -> first-exec P50/P99 end to end through the controller;
- sustained claims/sec and the density curve;
- pool-rebuild propagation across a multi-node pool;
- the competitor head-to-head (each competitor stood up on the same node);
- the bare-metal engine fork -> first-exec via `cmd/bench` (not run on the box
  this session);
- the **<= 10ms warm husk-pod activation TARGET** (#18/#15): the activate
  restore step is sub-10 ms on the reference node, but the full activate measures
  ~27 ms P50 there, so the end-to-end target is not yet met;
- a pinned reference-node CI runner so all of the above regenerate on every run.

## Open (not yet measured)

These remain out of scope for the current harnesses and are tracked in
[#15](https://github.com/paperclipinc/mitos/issues/15) / roadmap section 4:

- **Bare-metal reference numbers** on the Hetzner + Talos reference node. A first
  reference run is recorded in the "Bare-metal reference node (#16)" section above
  (warm-claim activate P50 ~27 ms, restore ~6-16 ms, ~3 MiB marginal memory per
  fork), reproducible from `bench/husk-activate-latency.sh` and
  `bench/results/2026-06-13-bare-metal-husk.md`. The remaining bare-metal items
  are listed in "Items still needing the pinned bare-metal reference node (#16)"
  above.
- **Facade vs upstream in-VM resume head-to-head** on a KVM-capable kubelet (the
  #16 reference node): our warm husk re-activation vs the upstream reference
  controller's cold pod create, both timed to a serving endpoint on identical
  hardware. The `bench/facade/` harness measures the object-level resume on kind
  today; the in-VM tail is the bare-metal target (#16), as described in the
  "Facade vs upstream reference: resume latency" section above.
- **Latency regression gating**: deliberately not done on shared CI, which is
  too noisy to threshold without flaking.
