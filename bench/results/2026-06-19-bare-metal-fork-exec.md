# Bare-metal fork-to-first-exec: first formal cmd/bench run (#15)

Hardware: Hetzner dedicated (Intel Core i7-6700, 4c/8t, 64 GiB, NVMe), KVM enabled.
OS/cluster: Talos Linux v1.13.4, 2-node, Kubernetes v1.36.1, Flannel CNI.
Engine: Firecracker v1.15.0; `cmd/bench` in-process engine (zero jailer config,
the same construction as forkd). Mode `fork-exec`.
Template: `e2e-tmpl` (python:3.12-slim), snapshot mem 512 MiB; guest kernel
vmlinux 4.14.174 (the chart's default staged kernel).
Run: `cmd/bench -mode fork-exec -template e2e-tmpl -data-dir /var/lib/mitos
-kernel /var/lib/mitos/vmlinux -iterations 20 -warmup 3` as a privileged Job on a
KVM node (see `bench/fork-exec-job.yaml`).

## Measured (real, reproducible on this node)

`fork_to_first_exec` (wall time from fork start to the first exec result, N=20,
3 warmup iterations discarded; percentiles by `internal/benchstat`, nearest-rank):

| count | min | p50 | p90 | p99 | max | mean |
| --- | --- | --- | --- | --- | --- | --- |
| 20 | 97.54 ms | 103.88 ms | 107.28 ms | 109.37 ms | 109.37 ms | 103.65 ms |

Component (from the Firecracker logs during the run):
- `/snapshot/load` (engine restore): ~16 ms.
- The remainder is lazy page-fault-in of the restored guest memory plus the guest
  agent servicing the first exec (the "0.8 ms restore + lazy faults" tail
  documented in BENCHMARKS.md, here measured end to end).

## Contention-free validation (N=50, quiesced node, #16)

To check whether co-location with forkd inflated the number, the run was repeated
on a QUIESCED node: all pools/forks deleted (only the idle forkd daemon left) and
the node cordoned, then `cmd/bench -iterations 50 -warmup 5`:

| count | min | p50 | p90 | p99 | max | mean |
| --- | --- | --- | --- | --- | --- | --- |
| 50 | 77.91 ms | 104.04 ms | 109.75 ms | 112.12 ms | 112.12 ms | 103.68 ms |

The p50/mean are within noise of the co-located run (103.9/103.65 ms), so the
co-location caveat is IMMATERIAL: ~104 ms fork-to-first-exec is a robust,
contention-independent floor on this hardware, not an artifact. (The wider sample
surfaces a faster 77.9 ms best case.) A dedicated reference node would not move
the p50; the lazy page-fault tail (#167), not contention, is what dominates.

## Caveats (honest scope)

- Single template (python:3.12-slim), single node, 2015-era CPU. The
  page-fault-prefetch + hugepage work (#167) targets the lazy-fault tail that
  dominates this ~104 ms (vs the ~16 ms restore).
- `fork-exec` includes terminating the sandbox after the first exec; the clock
  stops at the first exec result (see BENCHMARKS.md methodology).

## Reproduce

Build `bench/fork-exec-job.yaml`'s image (`FROM mitos-forkd + cmd/bench`, see the
manifest header) and apply it on a cluster with a built template; read the Job
log for the percentile table.
