# Lazy live-cow restore: warm-claim activate on a real cluster (2026-07-09)

Source for the numbers the m7 lazy-restore change claims. Reproduced with
[`bench/husk-activate-latency.sh`](../husk-activate-latency.sh), the published
warm-claim activate harness, against a live Kubernetes cluster with the husk-pods
path and `--live-cow-fork` enabled.

## Hardware and configuration

| | |
|---|---|
| node | `mitos-kvm-1`, Hetzner robot bare metal, Intel i7-6700 (4 cores / 8 threads), 62 GiB RAM |
| OS | Talos, kernel 6.18 |
| pool | `python`, template `ghcr.io/mitos-run/mitos-python:v1.13.0`, guest 2 vCPU / 512 MiB, `warm.min: 8` |
| flags | `--multi-vm-fork --live-cow-fork --live-cow-child-import --prewarm-child --husk-conn-reuse` |
| command | `bench/husk-activate-latency.sh <kubeconfig> python mitos 11` |

Guest RAM is 512 MiB, so "eager" moves 512 MiB per restore by construction.

## Warm-claim activate latency (ms), N=11

| | eager copy (before) | lazy restore (after) |
|---|---|---|
| min | 202.73 | 57.93 |
| P50 | **212.39** | **76.20** |
| P95 | 245.87 | 135.48 |
| max | 245.87 | 135.48 |
| samples under 100 ms | 0 / 11 | 10 / 11 |

The remaining P95 outlier is warm-pool refill contention: replacing a claimed husk pod
boots a Firecracker VMM on the same 4 cores that are serving the activate. It is not
CPU-quota throttling (`cpu.stat: nr_throttled 0` in a claimed husk pod's cgroup).

## Where the time went (husk-stub stage timing, mean of 6 sustained claims)

| stage | eager | lazy |
|---|---|---|
| `vmstate_restore` | 195.5 | **12.8** |
| `guest_ready` | 19.3 | 24.9 |
| `handshake` | 6.5 | 16.3 |
| `egress_filter` | 30.9 | 13.3 |
| `resume` | 0.5 | 1.0 |
| total | 252.7 | 68.3 |

`vmstate_restore` is the `PUT /snapshot/load` call. Firecracker's own timer agrees:
`'load snapshot' API request took 194942 us` on the eager path. The cost does not vanish,
it moves: `guest_ready` and `handshake` now pay for the pages the guest actually touches.

## Memory per active sandbox

Node `Shmem` delta, measured by activating sandboxes one at a time:

| | eager | lazy |
|---|---|---|
| 1 active | +512 MiB | +72 MiB |
| 2 active | +1024 MiB | +144 MiB |
| 3 active | +1536 MiB | +216 MiB |

Exactly linear either way. The eager copy gave every VM its own private 512 MiB of shmem,
which is what falsified the "~3 MiB marginal memory per fork via CoW page sharing" claim.
`memory.current` of a claimed husk pod's cgroup is ~105 MiB on the lazy path.

## Populate granularity

A restoring guest touches SCATTERED pages, so a chunk larger than the access footprint
copies the whole chunk to satisfy one page. Measured on this node, same harness:

| chunk | faulted in | activate P50 |
|---|---|---|
| 2 MiB | ~194 MiB | 114.4 ms |
| 256 KiB | ~125 MiB | 89.6 ms |
| **64 KiB** | **~72 MiB** | **76.2 ms** |

A sequential-touch microbenchmark is misleading here: it reports 2 MiB as optimal because
perfect locality hides the amplification.

## Functional verification alongside the timings

- co-located `fromSandbox` fork: 391 to 615 ms, `1/1 husk forks ready` (eager: 375 to 524 ms;
  the delta is populate-on-freeze filling the residual chunks, by design)
- hosted path on `api.mitos.run`: `create -> run_code (42) -> fork -> child run_code
  ('child-ok 15 3.12.13')`
- 8/8 warm husk pods Running, 0 restarts

## Caveat on the published headline

The README quotes `~27 ms P50` warm-claim activate for the bare-metal reference node. This
run does not reproduce that on `mitos-kvm-1`: it reaches 76 ms P50. The gap is now
`guest_ready` (24.9 ms, mostly demand fault-in, which a hot-page preload before Resume
would cut) plus `handshake` (16.3 ms, which re-dials vsock because the readiness probe
closes the connection it just proved healthy) plus `egress_filter` (13.3 ms of process
startup). Those are tracked as follow-ups; the number above is what this hardware does today.
