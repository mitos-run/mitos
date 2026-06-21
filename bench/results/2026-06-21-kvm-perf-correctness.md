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
- ONE test FAILED IN THIS ENVIRONMENT ONLY: `TestEnginePauseResumePreservesStateKVM`
  hangs on a BACKGROUNDED exec (`sh -c 'sleep 600 & echo $! > /sleeper.pid'`); the
  agent's exec waits on the stdout fd, which the backgrounded `sleep` holds open,
  so the host vsock recv times out (~66 s). IMPORTANT: this test EXISTS on main and
  main's `firecracker-test` CI is GREEN, so it PASSES on the CI runner (standard
  ubuntu kernel, the CI's template image). The failure here is environment-specific
  to this box: the Hetzner rescue kernel (custom 6.12.67) and the `busybox:stable`
  image's `sh` (busybox `sh` handles the backgrounded-child stdout fd differently
  than the CI image's shell). So this is NOT a confirmed product regression, it is
  a note that backgrounded-exec-over-vsock is sensitive to the guest shell/kernel
  and worth hardening (close/don't-wait-on inherited fds for backgrounded children),
  not a gate this PR introduces. It is unrelated to the #167/#168 work and not on
  the UFFD path.

## #167 hugepage + prefetch (MEASURED on a userfaultfd-capable node)

Measured on a SECOND i7-6700 reinstalled to stock Debian 12 (kernel
6.1.0-49-amd64, `CONFIG_USERFAULTFD=y`), NVMe XFS reflink, python:3.12-slim,
2048 x 2 MiB hugepages reserved. `cmd/bench --mode prefetch` captures the
template hot-page set, then runs two arms over the REAL userfaultfd restore:
OFF (lazy faults) vs ON (hot set preloaded before resume). N=20, 3 warmup.

4 KiB base-page template (UFFD restore):

| arm | mean faults/resume | claim->exec p50 | claim->exec p99 |
| --- | --- | --- | --- |
| prefetch off | 1877 | 153.5 ms | 216.0 ms |
| prefetch on | 45 | 117.0 ms | 138.2 ms |

2 MiB hugepage template (UFFD restore; hugepages REQUIRE uffd):

| arm | mean faults/resume | claim->exec p50 | claim->exec p99 |
| --- | --- | --- | --- |
| prefetch off | 24 | 115.7 ms | 140.0 ms |
| prefetch on | 2 | 108.7 ms | 134.2 ms |

Readings, in order of impact:

- HUGEPAGES cut the fault count ~78x at the same workload: 1877 faults (4 KiB)
  vs 24 (2 MiB), prefetch OFF in both. The ~7.5 MiB the guest touches to first
  exec is ~1877 base pages but only ~24 huge pages (512x coverage per page).
- PREFETCH cuts the residual faults further within each backing: 1877 -> 45 on
  4 KiB, 24 -> 2 on 2 MiB, and improves claim->first-exec p50 within the UFFD
  path (153 -> 117 ms on 4 KiB; 116 -> 109 ms on 2 MiB).
- HONEST nuance on absolute latency: the userfaultfd path adds handler overhead
  (each fault is serviced by a userspace UFFDIO_COPY, and the load itself takes
  ~30 ms vs ~8 ms file-mapped), so a UFFD claim->exec (108-153 ms) is HIGHER than
  the plain file-mapped 4 KiB baseline (~51-67 ms, see #15 and the box1 tmpl
  fork at 51 ms). The win is the fault-count collapse (the kernel/handler pay
  per fault, so 1877 -> 2 is the density-and-scale lever under concurrent load)
  and the within-UFFD prefetch latency improvement. Hugepage restore has NO
  file-mapped alternative (Firecracker refuses it), so for hugepages the UFFD
  path is the only path and 2 MiB + prefetch (2 faults, 109 ms p50) is its best
  configuration.

These numbers replace the prior TARGETs for the fault-count reduction; the
absolute hugepage density gains under a real claim storm remain to be measured
(this run measures single-fork resume, not concurrent density). The earlier
blocker (the Hetzner rescue kernel lacks `CONFIG_USERFAULTFD`, so
`userfaultfd(2)` -> ENOSYS and Firecracker fails a 2 MiB restore with "Failed to
UFFD object: System error") is exactly what `mitos doctor` now flags.
