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

## Caveats (honest scope)

- The bench ran CO-LOCATED with the live forkd DaemonSet on the same node, so the
  numbers carry some contention noise (a dedicated, idle reference node per #16
  would tighten them). They are a real floor, not a tuned best case.
- Single template (python:3.12-slim), single node, 2015-era CPU. The
  page-fault-prefetch + hugepage work (#167) targets the lazy-fault tail that
  dominates this ~104 ms (vs the ~16 ms restore).
- `fork-exec` includes terminating the sandbox after the first exec; the clock
  stops at the first exec result (see BENCHMARKS.md methodology).

## Reproduce

Build `bench/fork-exec-job.yaml`'s image (`FROM mitos-forkd + cmd/bench`, see the
manifest header) and apply it on a cluster with a built template; read the Job
log for the percentile table.
