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
| probe | `print(1)` via `run_code` |
| pacing | 13 s between iterations (free tier allows 5 creates/minute) |
| httpx | 0.28.1 / httpcore 1.0.9 (the SDK requires `httpx>=0.27.0`) |
| command | `MITOS_API_KEY=... python3 bench/tti-latency.py 20` |

Measuring from a laptop across the public internet mostly measures the internet: an
unrouted 404 costs 250 ms or more from a random location and 6 ms from inside the
cluster. The httpx version is part of the setup, not a footnote: see below.

## Result, ms, N=20, 20/20 success

| | |
|---|---|
| min | **265.7** |
| P50 | **307.7** |
| P90 | 348.4 |
| P95 | 380.8 |
| max | 413.1 |
| mean | 315.1 |

Split: create median 195.5 ms, first exec median 117.8 ms.

## How it got here

The starting point was 495.0 ms P50. Every millisecond of the improvement is client
side; the server build never changed. Each defect was found by logging every HTTP
request together with the identity of the socket it went out on.

| | P50 |
|---|---|
| starting point | 495.0 |
| after the three client fixes (same environment) | 340.7 |
| after correcting the environment to a supported httpx | **307.7** |

### The three client fixes

| defect | cost |
|---|---|
| a streaming Connect RPC never returned its connection to the pool, because the reader stopped at the terminal frame and left the body short of EOF. `run_code` was worse: it stopped at the `exitCode` MESSAGE frame, so the end-stream frame was never read at all | ~70 ms per exec |
| the pool was keyed by `(base URL, timeout)`, and create uses a 60 s client while the first exec uses a 30 s one, so the two never shared a socket. In httpx the connections live in the transport, not the client | one extra TLS handshake per sandbox |
| `create()` called `ensure_template` before every fork, re-resolving the identical template each time | ~50 ms per create |

In the measured run afterwards, every request of every iteration (`/v1/templates`,
`/v1/fork`, `RunCodeStream`, `DELETE`) went out on the SAME socket for the whole
process, logged by socket identity. That is an observation, not a guarantee: a shared
transport gives a process-wide pool, and a server idle timeout, a connection failure, or
concurrent requests can still rotate the socket. The regression tests assert the
property that matters and can be guaranteed, by counting TCP accepts on an in-process
keep-alive server: three streaming RPCs must open exactly one connection.

### The environment: Nagle, and a 37 ms tax on every request

The 340.7 ms run was measured against `httpx 0.23.3`, which is BELOW the SDK's own
declared floor of `httpx>=0.27.0`. That turned out to matter a great deal.

`httpcore 0.16` (shipped with httpx 0.23) does not set `TCP_NODELAY`. Because httpcore
writes the request headers and the body as separate segments, the second one waits on
the peer's delayed ACK. Interleaved call by call over the same warm connection to the
same sandbox, alternating clients so that server-side drift hits both equally:

| client | `exec true` median | min |
|---|---|---|
| `http.client` (stdlib, sets `TCP_NODELAY`) | 25.1 ms | 19.8 ms |
| `httpx 0.23.3` (`httpcore 0.16.3`, Nagle ON) | 61.8 ms | 56.6 ms |
| `httpx 0.28.1` (`httpcore 1.0.9`, Nagle OFF) | 26.7 ms | 20.6 ms |

37 ms on EVERY request, which for an agent is every tool call. The SDK now passes
`socket_options=[(IPPROTO_TCP, TCP_NODELAY, 1)]` to the transport it owns, so the
invariant no longer depends on a transitive default. A test asserts `TCP_NODELAY` is
set on a pooled connection.

## Where the remaining 308 ms goes

| | ms | what it is |
|---|---|---|
| create | 196 | gateway, control plane, engine restore |
| first exec | 118 | gateway, vsock to the guest agent, and the Python interpreter |

The engine's own warm-claim activate is 60 to 76 ms P50 on this hardware
(`bench/husk-activate-latency.sh`, see
[2026-07-09-lazy-livecow-restore.md](2026-07-09-lazy-livecow-restore.md)), so it is a
minority of TTI. Most of what a user waits for is the Kubernetes control plane, not
Firecracker.

On the exec leg, a warm `exec true` (the cheapest possible guest round trip) costs
about 25 ms end to end from this client, of which the guest itself reports ~21 ms. The
Python interpreter accounts for most of the difference between that and `print(1)`.

## Peer set

Context, NOT a controlled comparison. The numbers below are P50s we computed from the
raw per-iteration data ComputeSDK publishes, run `2026-07-09T01:03:05Z`, from
`results/sequential_tti/2026-07-09.json` in `computesdk/benchmarks`. They were produced
on ComputeSDK's runner against each vendor's own hardware; we did not re-run them.

Reproduce with `python3 bench/peer-tti.py --date 2026-07-09`. Note that
`latest.json` on their default branch is MUTABLE, so a number published from it cannot
be re-derived later; the harness takes `--ref <commit-sha>` for that reason, and this
table names the frozen daily file it used.

`n` is the number of iterations that produced a valid sample and `err` the number that
did not, so a percentile computed over fewer than 100 samples is visible rather than
implied. Errored iterations are excluded, and they matter: `superserve` records
`ttiMs: 0.0` on its failed iteration, which would otherwise report a 0.0 ms minimum.

| provider | TTI P50 | P90 | min | n | err |
|---|---|---|---|---|---|
| isorun | 12.8 | 15.0 | 11.9 | 100 | 0 |
| declaw | 37.7 | 88.8 | 28.5 | 100 | 0 |
| northflank | 95.9 | 122.0 | 66.1 | 100 | 0 |
| daytona | 136.2 | 300.7 | 97.0 | 100 | 0 |
| archil | 201.0 | 372.7 | 133.9 | 100 | 0 |
| upstash | 258.3 | 275.9 | 235.6 | 100 | 0 |
| **mitos** | **307.7** | 348.4 | 265.7 | 20 | 0 |
| e2b | 365.6 | 501.1 | 288.8 | 100 | 0 |
| beam | 377.9 | 388.6 | 175.1 | 100 | 0 |
| vercel | 392.7 | 503.3 | 241.4 | 100 | 0 |
| modal | 470.9 | 603.9 | 429.8 | 100 | 0 |
| cloudflare | 1758.7 | 2139.8 | 1027.7 | 100 | 0 |
| codesandbox | 2215.8 | 2513.5 | 1938.5 | 100 | 0 |

Read that table with three caveats, all of which cut against us claiming a win from it:

1. **Different probe.** ComputeSDK runs `node -v`; our row runs `print(1)` through the
   Python template, which pays interpreter startup inside the guest.
2. **Different pacing.** ComputeSDK runs 100 iterations back to back. Our row is paced
   13 s apart to stay under the free tier's 5 creates/minute.
3. **Different clients and regions.** Their harness, their runner (`namespace-profile-default`),
   their network. Proximity to that runner is a large share of any provider's score.

Our row is therefore NOT a measured position in their leaderboard. It is our number,
from our harness, printed beside theirs for scale. Even the ordering against E2B
(307.7 ms vs 365.6 ms), the one peer documenting the same isolation primitive
(Firecracker microVMs), is directional and unverified: it was not produced by the same
harness on the same client. To claim a position we have to ship an adapter to
`computesdk/computesdk` `packages/mitos` and be measured by their runner.

## What the sequential table does and does not measure

ComputeSDK also publishes a BURST run (`results/burst_tti/latest.json`, same 100
iterations, fired concurrently). Dividing burst P50 by sequential P50 separates
providers that hand out a pre-warmed sandbox from providers that provision one per
request. Both computed from their raw data:

| provider | seq P50 | burst P50 | burst/seq |
|---|---|---|---|
| isorun | 12.8 | 48.2 | 3.8x |
| declaw | 37.7 | 225.8 | 6.0x |
| northflank | 95.9 | 189.0 | 2.0x |
| daytona | 136.2 | 705.0 | 5.2x |
| upstash | 258.3 | 2055.2 | 8.0x |
| e2b | 365.6 | 471.0 | 1.3x |
| modal | 470.9 | 565.4 | 1.2x |
| cloud-run | 433.3 | 506.4 | 1.2x |

The providers whose latency barely moves under burst (e2b, modal, cloud-run) are doing
the same amount of work per request either way. The providers that degrade several fold
are serving the sequential run from a warm pool that the burst run drains. That is a
property of the measurement, not an accusation: ComputeSDK's own methodology counts
"infrastructure allocation ... boot ... health check polling" inside TTI, and a pool
checkout skips all of it.

Two things follow, and both are about us:

- **mitos is a pool checkout too.** A warm claim activates an already-booted, dormant
  husk pod. So the comparison against Daytona (also 5.2x under burst) is a fair fight
  between two checkouts, and our checkout is simply more expensive today.
- **We would degrade badly under their burst run.** The prod pool is `warm.min: 8` on a
  single KVM node, and a drained pool refills by booting a VM. That is a capacity fact
  to fix before submitting an adapter, not a number to quote.

We have not verified the isolation model of the faster entries and make no claim about
it here.

ComputeSDK also publishes a `snapshot-fork` benchmark, but it measures OBJECT STORAGE
(S3, R2, Azure Blob, Tigris), not VM fork, so there is still no public apples-to-apples
peer number for the primitive mitos is actually built on.

Firecracker's own project publishes no snapshot-restore TTI number either.

The gap to the fastest entries is control-plane cost, not engine cost, and is tracked
in #871.
