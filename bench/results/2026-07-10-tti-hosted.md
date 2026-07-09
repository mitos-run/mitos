# Time to Interactive on the hosted API (2026-07-10)

Source for the end-to-end latency numbers: the time from `create()` to a command
having actually RUN inside the sandbox and returned. Reproduce with
[`bench/tti-latency.py`](../tti-latency.py).

TTI is the only figure that is apples-to-apples with the public
[ComputeSDK benchmark](https://github.com/computesdk/benchmarks), which every hosted
sandbox vendor is measured against. It is NOT the same thing as the warm-claim
activate in [`bench/husk-activate-latency.sh`](../husk-activate-latency.sh), which
times only the microVM restore and excludes the client, the API, and the exec.
Quoting the engine number against a competitor's create-API number is a category
error.

## Setup

| | |
|---|---|
| API | `https://api.mitos.run` (hosted, free tier) |
| client | `mitos-bench1`, Hetzner bare metal, same region as the API |
| template | `python`, guest 2 vCPU / 512 MiB |
| probe | `print(1)` via `run_code`, matching ComputeSDK's `node -v` |
| pacing | 13 s between iterations (free tier allows 5 creates/minute) |
| command | `MITOS_API_KEY=... python3 bench/tti-latency.py 20` |

Measuring from a laptop across the public internet mostly measures the internet: an
unrouted 404 costs 250 ms or more from a random location and 6 ms from inside the
cluster.

## Time to Interactive, ms, N=20

| | before | after |
|---|---|---|
| min | 460.2 | **273.4** |
| P50 | 495.0 | **340.7** |
| P90 | 555.6 | 373.3 |
| P95 | 567.8 | 438.2 |
| max | 613.1 | 448.7 |
| mean | 510.0 | 343.8 |
| success | 20/20 | 20/20 |

Split after: create median 231.5 ms, first exec median 102.1 ms.

"Before" and "after" ran against the SAME server build. Every millisecond of the
difference is client side, found by logging each HTTP request with the identity of
the socket it went out on:

| defect | cost |
|---|---|
| a streaming Connect RPC never returned its connection to the pool (body not read to EOF), so every exec re-handshaked TLS | ~70 ms per exec |
| the pool was keyed by `(base URL, timeout)`, so create (60 s client) and the first exec (30 s client) never shared a socket | one extra handshake per sandbox |
| `create()` re-resolved the identical template on every call | ~50 ms per create |

After the fixes, all four requests of an iteration (`/v1/templates`, `/v1/fork`,
`RunCodeStream`, `DELETE`) ride ONE socket for the whole process.

## Where the remaining 341 ms goes

| | ms | what it is |
|---|---|---|
| create | 231 | gateway ~20, control plane ~139, engine restore 60 to 76 |
| first exec | 102 | gateway, vsock to the guest agent, and the Python interpreter |

The engine's own warm-claim activate is 60 to 76 ms P50 on this hardware
(`bench/husk-activate-latency.sh`, see
[2026-07-09-lazy-livecow-restore.md](2026-07-09-lazy-livecow-restore.md)). It is
therefore a minority of TTI: most of what a user waits for is the Kubernetes control
plane, not Firecracker.

## Peer set

Context, NOT a controlled comparison. The numbers below are P50s we computed from the
raw per-iteration data ComputeSDK publishes, run `2026-07-09T01:03:05Z`, 100
iterations per provider, `results/sequential_tti/latest.json` in
`computesdk/benchmarks`. They were produced on ComputeSDK's runner against each
vendor's own hardware; we did not re-run them.

| provider | TTI P50 | P90 | min |
|---|---|---|---|
| isorun | 12.8 | 15.0 | 11.9 |
| declaw | 37.7 | 88.8 | 28.5 |
| northflank | 95.9 | 122.0 | 66.1 |
| daytona | 136.2 | 300.7 | 97.0 |
| archil | 201.0 | 372.7 | 133.9 |
| upstash | 258.3 | 275.9 | 235.6 |
| **mitos** | **340.7** | 373.3 | 273.4 |
| e2b | 365.6 | 501.1 | 288.8 |
| beam | 377.9 | 388.6 | 175.1 |
| vercel | 392.7 | 503.3 | 241.4 |
| modal | 470.9 | 603.9 | 429.8 |
| cloudflare | 1758.7 | 2139.8 | 1027.7 |
| codesandbox | 2215.8 | 2513.5 | 1938.5 |

Read that table with three caveats, all of which cut against us being able to claim a
win from it:

1. **Different probe.** ComputeSDK runs `node -v`; our row runs `print(1)` through the
   Python template, which pays interpreter startup inside the guest.
2. **Different pacing.** ComputeSDK runs 100 iterations back to back. Our row is paced
   13 s apart to stay under the free tier's 5 creates/minute, so it never benefits from
   a hot path warmed by the previous iteration.
3. **Different clients and regions.** Their harness, their runner, their network.

The only clean statement available today is the ordering against E2B, the one peer that
documents the same isolation primitive (Firecracker microVMs): mitos 340.7 ms P50 vs
E2B 365.6 ms. We have not verified the isolation model of the faster entries, and the
sub-40 ms rows (isorun, declaw) are far below the cost of restoring a microVM at all on
this hardware, so they are almost certainly not booting one per request.

ComputeSDK also publishes a `snapshot-fork` benchmark, but it measures OBJECT STORAGE
(S3, R2, Azure Blob, Tigris), not VM fork, so there is still no public apples-to-apples
peer number for the primitive mitos is actually built on.

Firecracker's own project publishes no snapshot-restore TTI number either.

The gap to the fastest entries is control-plane cost, not engine cost, and is tracked
in #871.
