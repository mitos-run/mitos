# TTI create-path ladder: measuring each change in isolation (2026-07-12)

Method: identical to [2026-07-10-tti-hosted.md](2026-07-10-tti-hosted.md).
`bench/tti-latency.py` from `mitos-bench1` (Hetzner bare metal, same region as
the API), venv httpx 0.28.1 / httpcore 1.0.9, `python` template, `print(1)`
probe, 13 s pacing. Each rung re-measures prod AFTER exactly one deployed
change, so effects attribute to a release instead of to drift. Long runs
execute detached on the client (`systemd-run`), because an ssh-tethered run
dies with its session.

## The ladder

| rung | prod version | change under test | N | TTI P50 (ms) | create med (ms) | first exec med (ms) | success |
|---|---|---|---|---|---|---|---|
| A | v1.39.1 | baseline (2026-07-11) | 20 | 327.2 | 189.3 | 136.8 | 20/20 |
| B | v1.40.0 | gateway round-trip cuts (#895) | 20 | 349.0 | 191.4 | 166.4 | 20/20 |
| B' | v1.40.0 | same, higher power | 100 | 349.9 | 190.9 | 163.1 | 96/100 |
| C | v1.41.0 + checkout | pre-claimed checkout (#896) | 20 | 254.9 | **32.9** | 210.0 | **16/20** |

Full per-iteration outputs for each rung are reproducible with the harness
invocation from the method doc
(`MITOS_API_KEY=... python3 bench/tti-latency.py <N>`, environment per
[2026-07-10-tti-hosted.md](2026-07-10-tti-hosted.md)); the harness prints them
and this table quotes its summary lines.

## Rung B read honestly: two effects cancelled

Client-observed create was FLAT (189.3 to 190.9 median) across the v1.40.0
roll, despite #895 removing two serialized apiserver round trips from the
gateway create path. The controller's claim stage logs explain it: the
warm-claim `activate_rpc` stage regressed from 98.2 ms P50 (v1.39.1, n=33
Ready claims) to 118.8 ms P50 (v1.40.0, n=100), max 137, a uniform +20 ms
shift inside the husk activate round trip. v1.40.0 carries the default-off
prepare-time-restore slices; the flag-off path is meant to be inert, and the
observation is flagged with this data on the PR that landed it (#892). The
#895 cut is real but smaller than both this regression and the harness's
run-to-run noise (P50 drifted 307.7 to 327.2 between identical baseline runs
a day apart), which is why a 20-iteration run cannot resolve it: treat
sub-20 ms claims from N=20 sequential runs as unproven, per the
no-unverified-claims rule.

## Rung B' failures: 4/100 client read timeouts, server-side invisible

Four iterations failed with a client read timeout. For the same window the
controller's stage log shows ALL 100 claims Ready with reconcile totals of at
most 172 ms, and the gateway logged an INFO forward line for every request
with zero errors. The hang therefore sits past the gateway's forward entry
(create response path, runtime proxy, or exec stream) and cannot currently be
attributed, because the gateway forward log records neither status nor
duration. Filed as the observability gap it is (#901); #868 (client-correlated
fork latency logging) landed the same day and covers part of the fork path.

## Rung C: the create win is proven, the exec leg is the new problem

The pre-claimed checkout (#896) serves an eligible create from a buffer of
already-activated sandboxes: the hot path becomes the gateway front plus ONE
resourceVersion-guarded label patch, skipping the Sandbox write, the watch
wake-ups, the reconcile, and the (currently regressed) activate entirely;
buffered sandboxes pay activation at refill time, off the hot path.

Measured 2026-07-13 with `gateway.checkout.pools={python}` on v1.41.0: every
buffer-served create landed at 25 to 45 ms (median 32.9, against 190.9 on the
classic path), and the controller log confirms the refill loop replaced each
popped entry within its 15 s cadence. The design does what it claims.

Two things stop this rung from being the headline:

1. **First exec regressed to 210 ms median.** A buffered sandbox idles minutes
   before its first exec (oldest-first pop), and a controlled repro (create,
   idle 360 s, first exec, three rounds each, no buffer involved) shows the
   FIRST exec against an idle-activated sandbox costs 228 to 302 ms against
   122 to 294 ms fresh: idle working-set decay, roughly a 2x tax, filed with
   the data as #903. This affects ANY create-then-think-then-exec user, not
   just the buffer; the buffer merely pays it every time.
2. **4/20 iterations failed with client read timeouts** (the rung B' signature
   at 5x the rate). Server-side is clean for every failure (all claims Ready
   in at most 179 ms, all requests forwarded, zero errors), so attribution
   needs the #901 gateway instrumentation. The idle repro produced 0 stalls in
   6, so idleness alone does not explain them (#903 finding 2).

The checkout is therefore DISABLED in the hosted deployment again (config
revert, same day) until #903 is root-caused: a 20 percent iteration failure
rate is not a shippable trade for a 158 ms create cut, and the boring-failure
principle outranks the leaderboard. The burst-shaped fallback measurement is
deferred to the re-enable. Net honest position after rung C: the architecture
hits its latency target and the remaining work is an exec-leg bug plus
instrumentation, both filed and owned.

## Peer context

The public peer table and its caveats are unchanged from
[2026-07-10-tti-hosted.md](2026-07-10-tti-hosted.md): our rows are our
harness beside their published numbers, not a measured leaderboard position.
The targets remain Daytona (136.2 ms P50) and Northflank (95.9 ms P50).
