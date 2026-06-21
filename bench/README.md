# Running the benchmark harness

`cmd/bench` is the reproducible source behind every latency number the project
publishes. It drives the real KVM-backed fork engine in-process and measures the
fork + vsock + guest-agent data path directly. This page is how to run it on a
real KVM host to reproduce the CI numbers or capture reference-hardware numbers.

For methodology and the meaning of each mode, see
[`../BENCHMARKS.md`](../BENCHMARKS.md).

## Requirements

- A Linux host with `/dev/kvm` (bare metal, or a VM with nested virt). The
  engine validates `/dev/kvm` at construction, so the tool builds and parses
  flags everywhere but only runs the timing path on a KVM host.
- A Firecracker binary (the CI pins v1.15.0).
- A guest kernel (`vmlinux`) and a template snapshot already laid out under the
  data dir (see below).

## Template layout the engine loads from

`cmd/bench --template <id> --data-dir <dir>` forks from a snapshot the engine
expects at:

```
<data-dir>/templates/<id>/snapshot/mem
<data-dir>/templates/<id>/snapshot/vmstate
<data-dir>/templates/<id>/rootfs.ext4      # the backing rootfs
<data-dir>/templates/<id>/verified         # cheap "trusted" marker (see below)
```

The snapshot must have been created with a **relative** vsock `uds_path`
(`vsock.sock`); the engine resolves it against each fork's own working
directory, so a relative path is required for forks not to collide on one host
socket. The rootfs must contain the guest agent as `/init` and a shell, so the
bench's exec (`/bin/sh -c /bin/true` inside the guest) resolves.

The cleanest way to produce this layout is to build the template through the
engine itself (`forkd`'s `CreateTemplate`, which boots the VM, snapshots it,
content-addresses it into the CAS store, and writes the `verified` marker). If
you instead lay out the snapshot files by hand (as the CI bench phase does),
the engine will refuse to fork an unverified snapshot; `touch
<data-dir>/templates/<id>/verified` tells the Fork-time gate the template is
trusted for that run. The full snapshot-create + layout sequence the CI uses is
in the "Bench harness" step of `.github/workflows/kvm-test.yaml`.

## Run it

```sh
go build -o /tmp/bench ./cmd/bench/

# fork -> first exec (cold-claim-shaped)
/tmp/bench \
  --mode fork-exec \
  --template <id> \
  --data-dir <data-dir> \
  --firecracker /usr/local/bin/firecracker \
  --kernel <data-dir>/vmlinux \
  --iterations 100 --warmup 10 \
  --summary --json fork.json

# warm exec round-trip (hot path)
/tmp/bench \
  --mode exec-rt \
  --template <id> \
  --data-dir <data-dir> \
  --firecracker /usr/local/bin/firecracker \
  --kernel <data-dir>/vmlinux \
  --iterations 100 --warmup 10 \
  --summary --json execrt.json

# 1-to-N live fork fan-out (issue #207): fork ONE warmed base into N children
/tmp/bench \
  --mode fork-fanout \
  --template <id> \
  --data-dir <data-dir> \
  --firecracker /usr/local/bin/firecracker \
  --kernel <data-dir>/vmlinux \
  --fanout-n 1,4,16,64 \
  --summary --json fanout.json
```

`fork-fanout` forks one base into N children at each N in `--fanout-n` (default
`1,4,16,64`), measuring each child's fork -> first-exec on a shared wall clock. It
reports, per N, the per-child time-to-ready distribution and the
wall-clock-to-N-ready (the headline number for the sub-second 1-to-N COW fan-out
claim). For a representative number the template snapshot must be a WARMED base
(repo loaded, deps installed) so each child forks useful state; see the fan-out
section in [`../BENCHMARKS.md`](../BENCHMARKS.md) for the prerequisites and the
honest writeup scaffold. The result JSON carries the raw per-child samples
alongside the summary. Aggregation is the pure, unit-tested
`internal/benchstat.AggregateFanOut`.

`--summary` prints the count/min/p50/p90/p99/max/mean table to stdout. `--json`
writes the same distribution as machine-readable JSON (durations in
nanoseconds) so results can be archived or diffed across hardware.

## Flags

| flag | meaning |
| --- | --- |
| `--mode` | `fork-exec`, `exec-rt`, `metering`, or `fork-fanout` |
| `--iterations` | measured iterations (default 50) |
| `--warmup` | warmup iterations, discarded (default 5) |
| `--template` | template (snapshot) id under the data dir (required) |
| `--data-dir` | data directory holding template snapshots |
| `--firecracker` | Firecracker binary path |
| `--kernel` | guest kernel path |
| `--fanout-n` | `fork-fanout` mode: comma-separated fan-out widths N (default `1,4,16,64`) |
| `--json` | optional path to write results JSON |
| `--summary` | print the summary table to stdout |

## Capturing reference-hardware numbers

To capture bare-metal reference numbers (roadmap section 4 / issue #15), run the
two modes on the reference node with a higher iteration count (the runs above
use 100), archive both JSON files, and record the host (CPU, kernel, Firecracker
version, rootfs) alongside them so the numbers are reproducible and auditable.

## Controller-path harnesses (claim, sustained, pool-rebuild)

`cmd/bench` measures the in-process engine data path only. The controller + pool
path (issue #15 items 1-3) is measured by the `bench/claim` Go harness, wrapped
by three shell scripts that mirror the structure of the scripts above. They drive
a REAL cluster over a kubeconfig: with no reachable cluster they fail with a clear
message and produce NO number (CLAUDE.md operating principle 1). Numbers are
produced on the maintainer's hardware and recorded in `bench/results/`.

| script | mode | measures |
| --- | --- | --- |
| `claim-first-exec-latency.sh <kubeconfig> <pool> [ns] [iters]` | `claim-exec` | claim-create -> first-exec end to end through the controller, P50/P90/P99 |
| `sustained-claims-throughput.sh <kubeconfig> <pool> [ns] [rate] [duration] [max_concurrent]` | `sustained` | achieved claims/sec, peak concurrency, per-node density (sweep `[rate]` for the density curve) |
| `pool-rebuild-propagation.sh <kubeconfig> <pool> [ns]` | `pool-rebuild` | pool update -> all-nodes-snapshot-ready propagation (multi-node cluster) |

The harness can also be run directly:

```sh
go build -o /tmp/claim-bench ./bench/claim/
/tmp/claim-bench --mode claim-exec --kubeconfig "$HOME/.kube/config" --pool default --iterations 20
```

`--json <path>` writes the result distribution (claim-exec, pool-rebuild) or the
throughput object (sustained) as machine-readable JSON for archiving.

## Competitor comparison (scaffold + methodology)

`bench/competitors/` is the scaffold + methodology for the head-to-head against
E2B (self-hosted), Daytona (OSS), and Agent Sandbox + Kata (issue #15 item 5).
`run-comparison.sh <adapter.sh> [iters] [warmup]` measures every system by the
SAME create-sandbox -> first-exec method via an adapter. `adapters/mitos.sh` is
wired to this repo's own harness; the competitor adapters are placeholders a
reproducer fills in for their own deployment (they exit non-zero until then, so a
run can never emit a fabricated competitor number). The honesty rule (no invented
competitor numbers; vendor-published figures are labeled as such) is in
`bench/competitors/README.md`.

For the contested 1-to-N fan-out claim (issue #207),
`run-fanout-comparison.sh <adapter.sh> [n1,n2,...]` measures the SAME fan-out
shape (one warmed base into N children, reporting wall-clock-to-N-ready and the
per-child time-to-ready distribution) on each system. `adapters/mitos-fanout.sh`
is wired to `cmd/bench --mode fork-fanout`; `adapters/modal-fanout.sh` (Modal
snapshot/fork, the headline competitor), `adapters/daytona-fanout.sh`, and
`adapters/e2b-fanout.sh` (cold-start baseline) are placeholders that exit
non-zero until filled in. Modal is not self-hostable, so a Modal number comes
from its hosted service, not the reference node; that asymmetry is recorded with
any Modal figure. The fan-out comparison will plainly record whether mitos fork
beats Modal branching, and if it does not, that the wedge is self-hosting plus
per-fork network isolation rather than raw speed; no conclusion is pre-written.
