# Competitor comparison harness (scaffold + methodology)

This directory is the **scaffold** and **methodology** for the head-to-head
competitor comparison issue #15 item 5 asks for: mitos vs E2B (self-hosted),
Daytona (OSS), Agent Sandbox + Kata, and similar self-hostable sandbox systems,
on identical hardware, regenerated from in-repo scripts so a third party can
reproduce or refute the result.

It is deliberately a scaffold. Running a competitor requires standing up that
competitor's own stack, which is out of this repo's control and cannot be
checked into a `make test` path. What this directory provides:

1. A neutral driver contract every system's adapter implements
   (`run-comparison.sh`), so each system is measured by the SAME method.
2. A per-system adapter stub (`adapters/`) that documents exactly what a
   reproducer must deploy and what command exposes "create sandbox -> first
   exec" for that system.
3. This methodology doc, so the comparison is reproducible by anyone, not a
   number we assert.

## The honesty rule (non-negotiable)

Per CLAUDE.md operating principle 1 (no unverified claims) and issue #15:

- We publish a mitos number ONLY from our own harness in this repo
  (`bench/claim-first-exec-latency.sh` and `cmd/bench`), on documented hardware.
- We do NOT invent competitor numbers. Any competitor figure that this repo's
  scripts did not measure on the same hardware is labeled **vendor-published**
  (with a citation to the vendor's own published source) and is NOT presented as
  our measurement.
- A competitor figure becomes "our measurement" ONLY after a maintainer runs the
  adapter below against that system on the same reference hardware and records
  the raw output in `bench/results/`.

## The common method (every system, identical)

The single comparable metric is **create-sandbox -> first-exec** wall clock:
from the API call that asks the system for a fresh sandbox to the first
successful command execution inside it. This is the metric `cmd/bench`
(`fork_to_first_exec`) and `bench/claim-first-exec-latency.sh`
(`claim_to_first_exec`) already measure for mitos, so the comparison is
apples-to-apples.

For each system, on the SAME hardware:

1. Warm the system to its intended steady state (warm pool / pre-pulled image /
   pre-built snapshot), so the measured number reflects the system's designed
   hot path, not a one-time cold pull. Document what "warm" means for that
   system.
2. Run N sequential create -> first-exec iterations (default 20), discarding a
   small warmup (default 3).
3. Record min / P50 / P90 / P99 / max and the raw samples.
4. Record the host: CPU, kernel, the system's version, the sandbox image/size,
   and what warm state was pre-established.

`run-comparison.sh <adapter> <iterations> <warmup>` drives this loop against an
adapter; the adapter exposes two hooks: `warm` and `create_exec` (the latter
prints the create -> first-exec milliseconds for one iteration). See
`adapters/template.sh` for the contract and `adapters/mitos.sh` for the
reference implementation (which simply calls this repo's own harness).

## Systems and their reproduction prerequisites

| system | what a reproducer must deploy | "warm" state | source of any quoted number until measured |
| --- | --- | --- | --- |
| mitos (this repo) | a mitos cluster + warm SandboxPool, or a KVM host for `cmd/bench` | warm pool / loaded template snapshot | OUR harness (`bench/claim-first-exec-latency.sh`, `cmd/bench`) |
| E2B (self-hosted) | the open-source E2B infra stack on the same hardware | pre-built E2B template, warm sandbox pool if configured | vendor-published until a maintainer runs `adapters/e2b.sh` here |
| Daytona (OSS) | a self-hosted Daytona instance | pre-pulled workspace image | vendor-published until a maintainer runs `adapters/daytona.sh` here |
| Agent Sandbox + Kata | the upstream Agent Sandbox controller with the Kata runtime class | pre-pulled image, Kata runtime ready | vendor-published until a maintainer runs `adapters/agent-sandbox-kata.sh` here |

The adapter stubs in `adapters/` are intentionally non-functional placeholders
for the competitor systems: they print the exact deploy + invoke steps a
reproducer fills in, and exit non-zero so a comparison run can never silently
emit a fabricated competitor number. Only `adapters/mitos.sh` is wired to a real
harness (this repo's own).

## What ships in-repo vs what is a reproducer step

- In-repo and runnable now: the mitos side (`adapters/mitos.sh` -> this repo's
  harness) and the driver (`run-comparison.sh`).
- Reproducer step (not in-repo, by design): standing up each competitor and
  filling in its adapter, then running it on the same hardware. The result table
  is then assembled by hand in `bench/results/` from the raw per-system outputs,
  with every competitor figure either measured-here or labeled vendor-published.

## The 1-to-N fan-out comparison (issue #207)

The create -> first-exec metric above compares a single sandbox. Issue #207 adds
the **contested-claim** comparison: the 1-to-N live fork fan-out shape, where ONE
warmed base is fanned into N children. This is where mitos's defensible claim
lives (sub-second 1-to-N copy-on-write fan-out, self-hostable, per-fork network
isolation), and benchmarking only against E2B would overstate the edge, so we
measure the SAME fan-out shape on the systems that actually contest it.

The driver is `run-fanout-comparison.sh <adapter.sh> [n1,n2,...]` (default widths
`1,4,16,64`). A fan-out adapter defines `warm` and `fanout(N)`; `fanout(N)`
brings up N children from one warmed base and prints the wall-clock-to-N-ready on
line 1, then each child's time-to-ready on the following N lines. The driver
re-derives the per-child distribution and reports it next to the
wall-clock-to-N-ready. See `adapters/template-fanout.sh` for the contract and
`adapters/mitos-fanout.sh` for the reference (wired to `cmd/bench --mode
fork-fanout`).

| system | fan-out adapter | what its "fan-out" is | reproduction prerequisite | source until measured |
| --- | --- | --- | --- | --- |
| mitos (this repo) | `adapters/mitos-fanout.sh` | live COW fork of one warmed snapshot into N children | KVM host + a pre-built, verified WARMED template snapshot (repo + deps baked in) | OUR harness (`cmd/bench --mode fork-fanout`) |
| Modal Sandboxes | `adapters/modal-fanout.sh` | branch/fork one snapshot into N (the headline competitor) | a Modal account + a warmed Modal snapshot; NOT self-hostable, so run against Modal's hosted service | vendor-published until a maintainer runs the adapter |
| Daytona (OSS) | `adapters/daytona-fanout.sh` | N independent creates from one warmed image (cold-start baseline) | self-hosted Daytona on the reference node | vendor-published until a maintainer runs the adapter |
| E2B (self-hosted) | `adapters/e2b-fanout.sh` | N creates from one warmed template (cold-start baseline) | self-hosted E2B on the reference node | vendor-published until a maintainer runs the adapter |

### The honesty asymmetry to record (Modal)

Modal is not self-hostable, so a Modal fan-out number necessarily comes from
Modal's hosted service, not the same reference hardware as the mitos run. That is
NOT an apples-to-apples hardware comparison and must be recorded as such with any
Modal figure: the Modal number measures hosted fan-out, the mitos number measures
self-hosted fan-out on the reference node. Until `adapters/modal-fanout.sh` is
filled in and run, any Modal fan-out figure is **vendor-published**, not our
measurement.

### What this comparison will plainly record (no conclusion pre-written)

The fan-out comparison will record whether mitos fork actually beats Modal
branching on wall-clock-to-N-ready and per-child time-to-ready. **If mitos wins,
the wedge includes raw fan-out speed. If it does NOT win on raw speed, the wedge
is self-hosting plus per-fork network isolation, not speed.** No conclusion and no
number is written here in advance: the result is OPEN, to be filled on the #16
reference node from the raw per-system outputs, with every competitor figure
either measured-here or labeled vendor-published.

### Prerequisites for a fair fan-out number

A representative fan-out figure needs a WARMED base (repo loaded, deps installed)
so each child forks useful state, and at larger N a warm pool / autoscale plus
repo-preloaded templates so the host is not paying first-fork costs mid-fan-out.
The in-process `adapters/mitos-fanout.sh` path forks from one pre-built template
snapshot directly, isolating the engine fan-out cost; the end-to-end
controller-path fan-out depends on warm-pool autoscale and repo-preloaded
templates and is OPEN pending those plus the #16 reference node.
