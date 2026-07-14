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
| D | v1.42.1 prepare on | prepare-time restore + prefault (#892/#906/#911) | 100 | 329.9 | **140.2** | 188.6 | 99/100 |

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

## Rung D: prepare-time restore cuts create, warm-pool decay eats the exec leg

Rung D is the CLASSIC (non-checkout) path with the whole prepare-time chain on:
the dormant husk pod restores and resumes its guest AND runs the inert warm cell
at Prepare (#892 slice 2, #906 slice 3), so a claim pays only the fork
handshake. Measured 2026-07-14, v1.42.1, N=100, the same 13 s paced harness as
rungs A to C, from mitos-bench1. The bench org was granted the pro tier for the
run (no rate-limit retries; the pacing is unchanged for comparability).

**Create dropped 190.9 to 140.2 ms median**, a real ~50 ms cut, and the
server-side stage log explains all of it: on a matched claim the husk activate
total fell from 112.8 ms (the 2026-07-10 baseline) to 23.80 ms, with
`vmstate_restore` 22.9 to 0.00, `guest_ready` 40.6 to 1.24, and `resume` 2.4 to
0.00; the controller `activate_rpc` stage fell from 98 to 129 ms to 38.5 ms.
That is the prepare-time restore doing exactly what it was built to do: the
microVM restore and the demand fault-in are no longer on the claim.

**First exec REGRESSED 163 to 188.6 ms median**, and TTI is therefore flat
(327.2 to 329.9). The cause is #903 working-set decay, now hitting the WARM POOL
rather than only the buffer. The prefault warms the run_code kernel ONCE at
Prepare, but a warm husk pod sits dormant for minutes before a claim, and its
prefaulted pages are reclaimed in that window (the 1.24 ms `guest_ready` above
was a pod claimed seconds after a fleet recycle; a long-dormant pod's first exec
pays the decay). The checkout buffer got a 60 s keepalive for exactly this
(#908); the warm pool did not, so a warm-pool prefault-keepalive is the missing
piece and is issue #913. Prepare-time restore is a genuine create-leg
win that a decayed exec leg currently cancels on the classic path.

The lever that still targets Daytona is the checkout (rung E, next): it skips the
control-plane create cost entirely (rung C proved 32.9 ms create) AND its
buffered pods ARE keepalive-warmed now, so both legs should land. Rung D is the
honest record that prepare-time restore alone, without a warm-pool keepalive,
moves cost from the create leg to the exec leg rather than off TTI.

## Peer context

The public peer table and its caveats are unchanged from
[2026-07-10-tti-hosted.md](2026-07-10-tti-hosted.md): our rows are our
harness beside their published numbers, not a measured leaderboard position.
The targets remain Daytona (136.2 ms P50) and Northflank (95.9 ms P50).
