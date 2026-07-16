# Single-node burst capacity: how much parallelism one KVM node absorbs (2026-07-15)

The create-path ladder ([2026-07-12-tti-create-path-ladder.md](2026-07-12-tti-create-path-ladder.md))
measured the SEQUENTIAL hosted TTI (P50 96.8 ms). It deferred the burst
measurement, noting a concurrent burst would drain the single-node warm pool
(issue #586, #891). This is that burst measurement: how one KVM node behaves
when N `create -> run_code -> terminate` cycles fire at once.

## Method

`bench/burst-tti.py` from `mitos-bench1` (in region), `python` template,
`print(1)` probe. N threads release simultaneously on a barrier (a true burst,
not paced), each timing its own create-to-first-exec. The benchmark org holds
the pro tier so quota does not cap concurrency. Prod is v1.43.0 on ONE KVM node
(`mitos-kvm-1`, 64.8 GiB allocatable, husk pods request 2.5 GiB each, so the
hard ceiling is ~24 concurrent sandboxes).

Two configurations, to isolate the checkout-buffer lever:

- **baseline**: checkout `floor 2 / cap 4`, `warm.min 8` (~12 pre-activated).
- **deeper buffer**: checkout `floor 8 / cap 12`, `warm.min 8` (~16 pre-activated).

## Result

TTI P50 (ms), all iterations succeeded except where noted:

| burst N | baseline P50 | deeper-buffer P50 | baseline wall | deeper-buffer wall | success |
|---|---|---|---|---|---|
| 4 | 318 | 357 | 612 ms | 477 ms | 4/4, 4/4 |
| 8 | 583 | **545** | 1015 ms | **709 ms** | 8/8, 8/8 |
| 16 | 1132 | **905** | 7567 ms | 7114 ms | 16/16, 16/16 |
| 24 | 7477 | **1352** | 13768 ms | 56490 ms | 24/24, **23/24** |

Reproduce: `MITOS_API_KEY=... python3 bench/burst-tti.py <N>` per level.

## Reading it honestly

**The node degrades, it does not down.** Every burst up to 16 completed 100%,
and even the 24-wide burst completed 23 of 24. This is the #586 goal ("a node
loss or a bad pool degrades rather than downs") holding at the concurrency
layer: the scheduler queues the overflow behind typed backpressure rather than
OOMing the node.

**A deeper checkout buffer helps the MEDIAN across the realistic range.** At
N=8 the buffer of 8 pre-activated sandboxes absorbs the whole burst (P50 583 to
545, wall 1015 to 709), and the median at N=16 and N=24 drops sharply (1132 to
905; 7477 to 1352) because more of the burst is served from the fast checkout
path instead of booting per request.

**It does not lift the node's memory ceiling, and past ~16 the tail blows up.**
At N=24 the deeper buffer improved the median but made the TAIL worse (wall 13.8
to 56.5 s, and one create failed). The cause is physical: 24 concurrent
sandboxes need ~60 GiB of pod memory requests against a 64.8 GiB node, so the
overflow beyond the pre-activated set has almost no headroom to boot into, and
a deeper steady-state buffer leaves even less. So `floor 8` is a net win for
bursts up to ~16 (the node's comfortable range) but cannot make one node absorb
a 24-wide burst.

## What this says about the leaderboard (issue #891)

ComputeSDK's harness includes a ~100-wide concurrent burst. One KVM node cannot
serve that (100 * 2.5 GiB far exceeds any single node), so a single-node burst
row would degrade past ~16 concurrent, the same shape every single-node pool
shows (ComputeSDK measured Daytona degrading 5.2x under burst). The SEQUENTIAL
number (96.8 ms, below Daytona, level with Northflank) is honest and
leaderboard-comparable; a burst-COMPETITIVE submission needs multi-node warm
capacity, which is the full scope of #586. This measurement is the evidence for
that precondition, and the buffer-depth lever is the single-node half of it.

## Config left in place

The hosted deployment keeps the deeper buffer (`floor 8 / cap 12`), which
improves the common sequential-plus-small-burst load within the node budget.
The 24-wide saturation edge is documented, not fixed here; the fix is more than
one node (#586).
