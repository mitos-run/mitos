# KVM perf + correctness run (2026-06-21)

A real-KVM measurement and correctness pass run during the #167/#168/#15/#3
campaign. Every number below is reproducible from `cmd/bench` (operating
principle 1); nothing is a target dressed as a measurement, and the items that
could NOT be measured on this hardware are called out as such.

## Node

Hetzner dedicated, Intel Core i7-6700 (4 physical cores / 8 threads), 62 GiB RAM,
NVMe (XFS, `reflink=1`). Firecracker v1.15.0. Guest kernel vmlinux 6.1.155.
Template `smoke-tmpl` built from `python:3.12-slim` via `cmd/tmpl-smoke`, snapshot
mem 512 MiB. Data dir on the NVMe XFS partition so per-fork rootfs is reflink CoW
(matching production), not a full copy.

IMPORTANT KERNEL NOTE: this run used the standard 4 KiB file-backed restore. The
node's kernel for this run did NOT provide `userfaultfd`, so the #167
hugepage/prefetch path (which REQUIRES userfaultfd) was NOT measured here; see
the #167 section. This is the same CPU class as the earlier `#16` reference node
(`bench/results/2026-06-19-bare-metal-fork-exec.md`) but a different physical
instance.

## #15 baseline (measured)

`cmd/bench`, in-process engine (zero jailer), python:3.12-slim template.

fork-exec `fork_to_first_exec` (N=50, 5 warmup):

| count | min | p50 | p90 | p99 | max | mean |
| --- | --- | --- | --- | --- | --- | --- |
| 50 | 50.80 ms | 67.42 ms | 73.36 ms | 132.42 ms | 132.42 ms | 67.52 ms |

exec-rt `exec_round_trip` (N=100, 10 warmup):

| count | min | p50 | p90 | p99 | max | mean |
| --- | --- | --- | --- | --- | --- | --- |
| 100 | 1.452 ms | 1.515 ms | 1.574 ms | 1.674 ms | 1.707 ms | 1.527 ms |

metering, 8 forks of one template, settle 500 ms (CoW-aware):

- TotalUnique 10.16 MiB; UsedCoWAware 35.08 MiB; UsedNaive 209.27 MiB;
  CoWSavings 174.19 MiB. Per-fork unique ~1.27 MiB, shared ~24.9 MiB (counted
  once across the 8 forks). This is the #33 CoW density datapoint: 8 forks cost
  ~35 MiB resident, not ~209 MiB.

fork-fanout (per-child time-to-ready):

| N | wall-clock-to-all-ready | p50 per child |
| --- | --- | --- |
| 1 | 39.07 ms | 39.07 ms |
| 4 | 224.85 ms | 56.31 ms |
| 16 | 852.40 ms | 55.96 ms |

The fork-exec ~67 ms p50 is faster than the 2026-06-19 reference (~104 ms p50)
because of reflink (no rootfs full-copy), the 6.1.155 guest kernel, and the NVMe
data dir. `/snapshot/load` itself was ~8 ms; the remainder is the lazy page-fault
tail (#167) plus first-exec, which is exactly what the #167 work targets.

## #168 CPU pinning (measured, honest-neutral)

`cmd/bench --mode pinning`, real claim storm, pinning off vs on. See
`docs/perf/cpu-pinning.md` for the table and analysis. Summary: at 8 concurrent x
5 rounds AND 24 concurrent x 3 rounds, BOTH arms activated 100% (success-rate-lift
0.0%) and pinning ON was ~14 ms p50 slower. On a 4-core box the pin planner
fail-opens after 4 forks, and the real lever (the `SCHED_FIFO` launch-window
class) is gated to a nice-level bump, so the Browser Use launch-loss win is not
reproduced here. The driver and the feature are correct and fail-open; the win
needs the RT class and/or more cores.

## #3 / #163 fork correctness (real KVM)

`go test ./internal/fork/...` with the KVM asset env vars, real Firecracker:

- Real microVMs boot, snapshot create (~364 ms) + restore + fork all work; CPU
  vendor matches host on restore; the fork-triggered RNG reseed fires
  (`random: crng reseeded due to virtual machine fork`) -- fork-correctness row 1
  observed live.
- Guest agent unit tests (notify/reseed logic) PASS.
- All non-KVM `internal/fork` tests PASS (per-fork rootfs CoW isolation, volume
  reflink/share, network prepare/teardown, crash reconcile/adopt/reap, CAS
  pull/verify integrity gates).
- ONE genuine, reproducible FAILURE: `TestEnginePauseResumePreservesStateKVM`
  hangs on a BACKGROUNDED exec (`sh -c 'sleep 600 & echo $! > /sleeper.pid'`).
  The agent's exec waits on the stdout fd, which the backgrounded `sleep` holds
  open, so the host vsock recv times out (~66 s). The preceding non-backgrounded
  exec succeeds. This is a pre-existing guest-agent exec-semantics gap (handling
  of commands that background a child holding the output fd), independent of the
  #167/#168 work and not on the UFFD path. It must be resolved (or the test/agent
  exec contract reconciled) before the fork-correctness gate is green on KVM.

## #167 hugepage + prefetch (NOT measured here; blocked on kernel)

The hugepage-backed restore and the userfaultfd prefetch handler require a kernel
built with `CONFIG_USERFAULTFD=y`. The kernel available for this run lacked it
(`userfaultfd(2)` -> ENOSYS; Firecracker fails restore of a 2 MiB template with
"Failed to UFFD object: System error", and refuses a hugetlbfs file-map with
"Please use uffd"). The code is complete and unit-tested; the off-vs-on fault
count + claim->first-exec measurement (`cmd/bench --mode prefetch`) is pending a
stock-kernel node. `mitos doctor` now flags a missing userfaultfd. All #167
figures remain TARGETS (`docs/perf/snapshot-prefetch.md`).
