# Go vs Rust guest agent: bare-metal comparison (issue #310)

Head-to-head between the Go guest agent and the Rust spike agent (`guest/agent-rs/`)
on the SAME engine, SAME kernel, SAME rootfs, differing ONLY in the `/init`
binary. Both templates were built by the same procedure (`hack/rust-agent-rootfs.md`,
mirroring the `kvm-test.yaml` bench harness): boot a source VM with the agent as
`/init` plus busybox, snapshot, fork with `cmd/bench`.

## Host (reference hardware, bare metal, not shared CI)

- Box: mitos-bench1 (Hetzner dedicated), Intel Core i7-6700 @ 3.40GHz, 4 cores / 8 threads.
- Host kernel: 6.1.0-49-amd64 (Debian 12). Firecracker v1.15.0. Guest kernel: vmlinux (44279576 bytes).
- Go agent built `CGO_ENABLED=0 GOOS=linux GOARCH=amd64` (static). Rust agent built `x86_64-unknown-linux-musl` (static-pie), rustc 1.96.0.
- Bench: `cmd/bench`, 100 iterations, 20 warmup per run. Templates `bench-go` and `bench-rust`.

## Raw archives

Run 1 distributions (nanosecond samples) are archived next to this file:
`2026-06-23-spike-{go,rust}-{execrt,fork}.json`. The per-run p50/p90/p99 below
were captured from the `--summary` table; the reproducibility runs are recorded
inline rather than archived as separate JSON.

## exec round-trip (warm exec over an already-forked VM), ms

| run | Go p50 | Go p99 | Rust p50 | Rust p99 |
| --- | --- | --- | --- | --- |
| 1 | 0.703 | 0.887 | 0.611 | 0.691 |
| 2 | 0.602 | 0.741 | 0.606 | 0.747 |
| 3 | 0.720 | 0.816 | 0.595 | 0.734 |

Go p50 spans 0.602 to 0.720 ms; Rust p50 spans 0.595 to 0.611 ms. The
distributions OVERLAP: the Go run 2 median (0.602) is faster than two of the
three Rust medians. The apparent run-1 advantage was run-to-run noise. Honest
read: exec round-trip latency is statistically indistinguishable between the two
agents at p50. Rust shows a tighter spread (smaller run-to-run variance and a
more stable p99), consistent with the absence of a garbage collector, but the
median is not a win.

## fork to first exec (cold-claim-shaped: fork plus first exec), ms

| run | Go p50 | Go p99 | Go mean | Rust p50 | Rust p99 | Rust mean |
| --- | --- | --- | --- | --- | --- | --- |
| 1 | 114.346 | 144.701 | 113.803 | 100.556 | 121.224 | 100.484 |
| 2 | 113.198 | 135.984 | 113.284 | 100.354 | 128.518 | 101.050 |
| 3 | 114.428 | 134.793 | 113.530 | 98.943 | 124.968 | 100.395 |

Go p50 clusters at 113.2 to 114.4 ms; Rust p50 clusters at 98.9 to 100.6 ms. The
Rust agent is reproducibly faster on fork to first exec by about 13 ms at the
median, roughly 12 percent, across all three runs with no overlap. The
fork/restore engine is identical for both templates (same snapshot size, same
code path), so the delta lives in the resumed agent's first-exec handling: the
lean static Rust binary returns the first exec faster than the Go agent, whose
runtime and scheduler wake up after restore. This is the clearest signal in the
comparison.

## Binary size (the static /init binary)

| agent | bytes | note |
| --- | --- | --- |
| Go | 4,968,878 | ~4.74 MiB, static |
| Rust | 651,968 | ~637 KiB, static-pie musl |

The Rust agent is about 7.6x smaller. This matters for rootfs/image footprint
at husk-pool scale, not for steady-state RSS (below).

## Per-VM RSS (source VM, agent idle, firecracker process RSS), KB

| agent | RSS_KB |
| --- | --- |
| Go | 65,236 |
| Rust | 64,916 |

About 0.5 percent apart, within noise. At a 256 MiB guest the firecracker RSS is
dominated by touched guest pages and the VMM itself, not the agent, so swapping
the agent does NOT produce a material per-VM RSS reduction at this VM size. A
smaller-guest configuration might surface the agent's own footprint difference;
that was not measured here.

## Honest summary

- exec round-trip: no median win for Rust (within noise); tighter tail.
- fork to first exec: reproducible ~12 percent win for Rust (3 runs, no overlap).
- binary size: ~7.6x smaller for Rust.
- per-VM RSS: no material difference at a 256 MiB guest.

Single box (mitos-bench1), single session. Not yet reproduced on box2. The Rust
agent implements only the bench-exercised protocol subset and uses an uncredited
`/dev/urandom` reseed rather than the production `RNDADDENTROPY` path. See the
decision record in `docs/superpowers/decisions/2026-06-23-rust-guest-agent.md`.
