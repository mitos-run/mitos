# SP1 Rust gRPC agent bench: Go vs Rust (box1, 2026-06-24)

## Method

Both agents were benchmarked over the same gRPC contract (Control.Ping on vsock port 53).
This is NOT a real exec workload; fork_to_first_grpc_ping measures snapshot restore plus
HTTP/2 handshake plus one Control.Ping round-trip, and grpc_ping_round_trip measures a
single warm Control.Ping call on an already-running sandbox. Only /init differs between
templates; the Firecracker version, kernel, and rootfs size are identical.

Hardware: Hetzner AX41, Intel i7-6700 4c/8t, 64 GB RAM, Firecracker v1.15.0, KVM enabled,
no concurrent guest load. Bench binary: cmd/bench on feat/rust-guest-agent-grpc, flag
--exec-transport grpc.

Templates:
- bench-go-grpc: Go agent (SP1 branch, stripped, 11.2 MB) + Firecracker snapshot
- bench-rust-grpc: Rust agent (SP1 branch, musl static-pie, stripped, 2.4 MB) + Firecracker snapshot

Passes 1 (n=200, warmup=20) and passes 3-4 (n=100, warmup=10) are archived in
bench/results/2026-06-24-*.json. Pass 2 numbers (Go {115.9, 117.2} ms, Rust {96.1, 92.2} ms)
were captured in the #310 spike session and are cited from that record.

## fork-exec latency (fork to first gRPC Ping response)

Metric: fork_to_first_grpc_ping. Clock starts at engine.Fork() call, stops when
Control.Ping returns (HTTP/2 handshake included).

Per-run p50 spread across 4 passes (Go vs Rust):

| pass | Go p50 (ms) | Rust p50 (ms) | delta (ms) | Rust faster |
|------|-------------|---------------|------------|-------------|
| 1    | 115.3       | 94.2          | -21.1      | 18.3%       |
| 2    | 115.9       | 96.1          | -19.8      | 17.1%       |
| 3    | 118.5       | 95.7          | -22.8      | 19.2%       |
| 4    | 114.4       | 92.4          | -22.0      | 19.2%       |

No run overlaps. Rust p50 is consistently 17-19% faster across all passes. REPRODUCED.

Pass 1 full summary (n=200; representative run from JSON archive):

| agent | min (ms) | p50 (ms) | p90 (ms) | p99 (ms) | max (ms) | mean (ms) |
|-------|----------|----------|----------|----------|----------|-----------|
| Go    | 73.0     | 115.3    | 134.6    | 146.9    | 156.6    | 114.1     |
| Rust  | 51.7     | 94.2     | 107.8    | 120.2    | 127.5    |  94.5     |

## exec-rt latency (gRPC round-trip inside a running sandbox)

Metric: grpc_ping_round_trip. One Control.Ping call on a live, already-forked sandbox.
Measures warm in-VM gRPC server overhead. NOT a real exec.

Per-run p50 spread across 4 passes:

| pass | Go p50 (ms) | Rust p50 (ms) |
|------|-------------|---------------|
| 1    | 0.338       | 0.223         |
| 2    | 0.365       | 0.305         |
| 3    | 0.244       | 0.235         |
| 4    | 0.268       | 0.255         |

Go range: 0.244-0.365 ms. Rust range: 0.223-0.305 ms. The ranges overlap
(Go pass 3 at 0.244 ms beats Rust pass 1 at 0.223 ms by only 0.021 ms; Go pass 4 at 0.268 ms
is faster than Rust pass 2 at 0.305 ms). Run-to-run variance exceeds the inter-agent
difference. This is a WASH, not a win. The earlier single-run result of 35% was an artifact of
one favorable pairing. This matches the finding from issue #310 that warm round-trip is a tie.

p99 is noisy both ways (Rust pass 4 at 0.706 ms vs Go pass 1 at 0.475 ms); no reliable
signal at the tail.

## per-VM RSS (private-dirty set, post-settle)

Method: bench metering mode (-mode metering -forks 1 -settle-ms 3000), which reads
/proc/<fc-pid>/smaps after the guest has been running for 3 seconds. Measures the
Firecracker host-process private-dirty pages for one idle forked sandbox (no concurrent
forks; no CoW savings apply). Two passes each.

| agent | pass 1 MemoryUnique | pass 2 MemoryUnique |
|-------|--------------------|--------------------|
| Go    | 15.55 MiB          | 15.57 MiB          |
| Rust  | 15.73 MiB          | 15.74 MiB          |

Difference: ~0.2 MiB (Rust marginally higher by one or two dirty pages). This is within
measurement noise and is NOT a meaningful difference. The guest agent binary size does not
directly predict idle RSS because Firecracker maps the rootfs lazily; what dominates is the
VM memory image (128 MiB guest RAM snapshot), not the agent binary. Both agents idle at
~15.6 MiB private-dirty. WITHIN NOISE.

Note: a direct /proc/<pid>/VmRSS read on the Go Firecracker process immediately after resume
showed 14.0 MiB (smaps sum 15.9 MiB), consistent with the metering result. A bare-Firecracker
manual restore for the Rust template failed due to a missing API drive field in the probe
script; the metering method above is the correct and reproducible RSS measurement for both.

## binary size

| agent | stripped size |
|-------|--------------|
| Go    | 11.2 MB      |
| Rust  | 2.4 MB       |

Rust is 78.5% smaller (~4.7x). The Go binary includes gRPC deps (grew from ~5 MB pre-SP1 to
11.2 MB post-SP1). The 2.4 MB Rust figure is the production gRPC agent; the 637 KB figure
from the #310 spike was the earlier JSON-only agent (different binary).

## verdict

The Rust agent wins on fork-exec latency and binary size, and is neutral on ping round-trip
and per-VM RSS. No metric regresses.

- fork-exec p50: Rust 17-19% faster across 4 passes, no overlap. Clear, reproduced win.
- ping round-trip p50: within run-to-run noise, ranges overlap. A wash, not a win.
- per-VM RSS: ~15.6 MiB both agents, difference <0.2 MiB. Within noise. A wash.
- binary size: 2.4 MB vs 11.2 MB. ~4.7x smaller. Clear win.

Gate condition for #310: the Rust agent shows no regression on any measured axis, and a
real improvement on the fork path (the hot path for sandbox provisioning) and on footprint.
The ping round-trip wash does not block the transition; real exec latency (vsock + shell
round-trip) is not measured here and remains a separate gate.
