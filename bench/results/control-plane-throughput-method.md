# Control-plane throughput ceiling: method

This file records the METHOD for the control-plane throughput datapoint that the
`kind-e2e` CI job now produces on every run (issue #15 item 2). It deliberately
contains NO hardcoded numbers. Per CLAUDE.md operating principle 1 (no unverified
claims), the number is produced on a live cluster and published as a CI artifact,
never pasted here; when a reference hardware run traces the full density curve,
its distribution and host go in a dated result file next to this one (mirroring
`2026-06-13-bare-metal-husk.md`).

## What CI measures

The `kind-e2e` job stands up the MOCK control plane (`mitos dev up`: forkd
`--mock`, controller `--mock --disable-pki-bootstrap`) on ONE kind node, waits
for the `dev-default` `SandboxPool` to report a ready snapshot, then runs:

```
bench/claim --mode sustained --namespace default --pool dev-default \
  --rate 5 --duration 20s --max-concurrent 8 --json <artifact>
```

Each arrived claim is a `Sandbox` CR create, and the harness records its offset
to `status.phase=Ready`, the concurrency at that instant, and the node it landed
on. The aggregation is the unit-tested `internal/benchstat.AggregateThroughput`.
The JSON (`completed`, `window_ms`, `achieved_per_sec`, `peak_concurrent`,
`per_node_density`) is uploaded as the `control-plane-throughput-mock` artifact.

## What the number IS

The achieved claims/sec is the CONTROL-PLANE ceiling of this path, on this fixed
config: `Sandbox` CR create -> reconcile -> `NodeRegistry` node select -> mock
forkd `Fork` -> `status.phase=Ready`, observed by a watch. This is the
etcd / apiserver / reconciler throughput, the coupling that external scaling
critiques (running one Kubernetes control-plane transaction per sandbox) target.
It is a real, reproducible number produced by a live control plane.

## What the number is NOT

- NOT hardware density. The mock engine forks no real VM and holds no real
  guest memory, so nothing here reflects per-node fork memory (that is the CoW
  density datapoint in `BENCHMARKS.md`) or how many live sandboxes a real node
  packs.
- NOT the warm-husk activation path. On the mock/dev overlay claims fork on the
  raw-forkd mock engine; the pre-warmed husk-pod claim + in-place mTLS activation
  is the KVM tail proven in `kvm-test.yaml`, not this job.
- NOT a saturation ceiling. CI runs ONE operating point (rate 5/s, 8 concurrent)
  chosen to stay well inside a single kind node so the run is a stable
  correctness gate on shared runners. The knee (where achieved/sec stops tracking
  the arrival rate) is only visible by sweeping `--rate` upward.

## The CI gate

The gate is CORRECTNESS, not an absolute rate: shared GitHub runners vary too
much to assert a claims/sec threshold without flaking. CI asserts only that the
run completed, `completed > 0` (the end-to-end control-plane path drove claims to
Ready under load), and `achieved_per_sec > 0`. The rate itself is recorded, not
gated.

## Tracing the real density curve

On the reference KVM node (#16), against a warm pool sized for the target
concurrency, run `bench/sustained-claims-throughput.sh` several times sweeping
`--rate` (and `--max-concurrent`), and record each
`(rate -> achieved/sec, peak density, per-node density)` point plus the host
(CPU, kernel, Firecracker version, node count, pool size) in a new dated result
file. The p99 degradation point is where achieved/sec stops tracking the target
arrival rate. Do not publish a number this harness did not produce.
