#!/usr/bin/env bash
#
# networked-live-fork-latency.sh
#
# Reproducible source for the networked live-fork latency numbers (issue #336).
# Any published number MUST be reproducible from this script (CLAUDE.md
# operating principle 1): numbers live only in bench/results/ next to the
# hardware context.
#
# What it measures:
#
#   ARM 1 - cold-fork baseline (non-networked):
#     Wall clock from fork start to the first successful Control.Ping response
#     over gRPC (AgentGRPCPort 53), measured by cmd/bench --mode fork-exec.
#     This is the established isolated engine baseline (no per-fork networking
#     in the path). It lower-bounds the networked cold-fork path and is the
#     reference arm every live-fork improvement is measured against.
#
#   ARM 2 - networked live-fork end-to-end (upper bound):
#     Wall clock for a complete run of cmd/live-fork-egress-smoke, which
#     boots a networked source sandbox through the per-sandbox egress proxy
#     (--enable-networking + --egress-proxy + --proxy-sentinel + --proxy-port),
#     runs engine.ForkRunning on the live source, delivers the fork-correctness
#     and network handshake to the child (fresh MAC, IP, reseed, clock step),
#     and asserts independent egress on a fresh upstream connection at the stub.
#     Each measured iteration is an independent run of the binary (the binary
#     creates its own template and source per run). The timing is therefore an
#     UPPER BOUND on the isolated ForkRunning latency: it includes template
#     creation, source fork, proxy and network setup, live fork, correctness
#     handshake, and the egress assertions. A clean comparison against arm 1
#     requires extending cmd/bench with a live-fork-networked mode that sets up
#     the template and source once and times only the ForkRunning + handshake
#     (tracked by issue #336).
#
#   ARM 3 - N-way COW fan-out baseline (cold-fork, non-networked):
#     Wall clock until all N children of one template snapshot are ready plus
#     per-child time-to-ready distribution, measured by cmd/bench --mode
#     fork-fanout across the fan-out widths given by --fanout-n. This is the
#     established engine-level fan-out baseline. Networked live-fork fan-out
#     (N children from one live networked source via concurrent ForkRunning
#     calls) requires extending cmd/bench.
#
# Requirements:
#   - A Linux host with /dev/kvm (bare metal or nested virt with KVM
#     passthrough). On a non-KVM host cmd/bench and cmd/live-fork-egress-smoke
#     both exit non-zero at engine construction with a clear message, so this
#     script never produces a fabricated off-KVM number.
#   - Firecracker binary (CI pins v1.15.0; default /usr/local/bin/firecracker).
#   - A guest kernel (vmlinux).
#   - A busybox rootfs image (rootfs.ext4) with the guest agent as /init and
#     busybox providing wget, nc, ip, and awk. The same rootfs as kvm-test.yaml.
#   - The guest agent binary (agent-rs, built for the guest Linux/amd64 arch;
#     injected as /init when the template is built from the image).
#   - Host network stack for arm 2: tap device creation, nftables (for per-fork
#     DNAT and egress rules), and IP forwarding. Run as root or with the
#     required capabilities.
#   - go on PATH (the script builds cmd/bench and cmd/live-fork-egress-smoke
#     from the repo; no prior `make build` step is needed).
#   - python3 on PATH (parses the fan-out JSON emitted by cmd/bench --json).
#
# Usage:
#   sudo bench/networked-live-fork-latency.sh \
#     --image <rootfs.ext4> \
#     --data-dir <dir> \
#     --kernel <vmlinux> \
#     --agent-bin <agent-rs> \
#     [--firecracker <path>] \
#     [--proxy-sentinel <ip>] \
#     [--proxy-port <port>] \
#     [--iterations <n>] \
#     [--warmup <n>] \
#     [--fanout-n <w1,w2,...>]
#
# Example (the reference-node run):
#   sudo bench/networked-live-fork-latency.sh \
#     --image /tmp/mitos-test/rootfs.ext4 \
#     --data-dir /tmp/mitos-test/data \
#     --kernel /var/lib/mitos/vmlinux \
#     --agent-bin /tmp/mitos-test/agent
#
# Numbers are produced by the REAL KVM-backed engine; there are no expected or
# hardcoded latency figures in this script (CLAUDE.md operating principle 1).
# Record the output in bench/results/ with the host spec, Firecracker version,
# kernel version, and rootfs contents so the number is auditable.
#
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO="$(cd "${HERE}/.." && pwd)"

# ---- flag defaults ----
IMAGE=""
DATA_DIR=""
KERNEL=""
AGENT_BIN=""
FIRECRACKER="${FIRECRACKER:-/usr/local/bin/firecracker}"
PROXY_SENTINEL="${PROXY_SENTINEL:-169.254.169.2}"
PROXY_PORT="${PROXY_PORT:-3128}"
ITERATIONS="${ITERATIONS:-11}"
WARMUP="${WARMUP:-2}"
FANOUT_N="${FANOUT_N:-1,4,16}"

usage() {
  echo "usage: $0 --image <ext4> --data-dir <dir> --kernel <vmlinux> --agent-bin <agent> [opts]" >&2
  echo "  --firecracker <path>     firecracker binary (default: ${FIRECRACKER})" >&2
  echo "  --proxy-sentinel <ip>    fork-stable sentinel proxy address (default: ${PROXY_SENTINEL})" >&2
  echo "  --proxy-port <port>      egress proxy port (default: ${PROXY_PORT})" >&2
  echo "  --iterations <n>         measured iterations (default: ${ITERATIONS})" >&2
  echo "  --warmup <n>             warmup iterations discarded from stats (default: ${WARMUP})" >&2
  echo "  --fanout-n <csv>         fan-out widths for arm 3 (default: ${FANOUT_N})" >&2
  exit 2
}

while [ "$#" -gt 0 ]; do
  case "$1" in
    --image)         IMAGE="$2";         shift 2 ;;
    --data-dir)      DATA_DIR="$2";      shift 2 ;;
    --kernel)        KERNEL="$2";        shift 2 ;;
    --agent-bin)     AGENT_BIN="$2";     shift 2 ;;
    --firecracker)   FIRECRACKER="$2";   shift 2 ;;
    --proxy-sentinel) PROXY_SENTINEL="$2"; shift 2 ;;
    --proxy-port)    PROXY_PORT="$2";    shift 2 ;;
    --iterations)    ITERATIONS="$2";    shift 2 ;;
    --warmup)        WARMUP="$2";        shift 2 ;;
    --fanout-n)      FANOUT_N="$2";      shift 2 ;;
    -h|--help)       usage ;;
    *) echo "unknown flag: $1" >&2; usage ;;
  esac
done

if [ -z "$IMAGE" ] || [ -z "$DATA_DIR" ] || [ -z "$KERNEL" ] || [ -z "$AGENT_BIN" ]; then
  echo "error: --image, --data-dir, --kernel, and --agent-bin are required" >&2
  usage
fi

for req in go python3; do
  command -v "$req" >/dev/null 2>&1 || { echo "error: $req not found on PATH" >&2; exit 1; }
done

if [ ! -f "$IMAGE" ]; then
  echo "error: rootfs image not found: $IMAGE" >&2; exit 1
fi
if [ ! -d "$DATA_DIR" ]; then
  echo "error: data-dir does not exist: $DATA_DIR" >&2; exit 1
fi
if [ ! -f "$KERNEL" ]; then
  echo "error: kernel not found: $KERNEL" >&2; exit 1
fi
if [ ! -f "$AGENT_BIN" ]; then
  echo "error: agent-bin not found: $AGENT_BIN" >&2; exit 1
fi
if [ ! -f "$FIRECRACKER" ]; then
  echo "error: firecracker binary not found: $FIRECRACKER" >&2; exit 1
fi

echo "networked-live-fork-latency: iterations=$ITERATIONS warmup=$WARMUP fanout-n=$FANOUT_N"
echo "  image=$IMAGE"
echo "  data-dir=$DATA_DIR"
echo "  kernel=$KERNEL"
echo "  agent-bin=$AGENT_BIN"
echo "  firecracker=$FIRECRACKER"
echo "  proxy-sentinel=$PROXY_SENTINEL proxy-port=$PROXY_PORT"
echo

# ---- build binaries ----
echo "building bench binaries from repo ..."
BENCH_BIN="$(mktemp -t networked-live-fork-bench.XXXXXX)"
LFE_BIN="$(mktemp -t networked-live-fork-egress.XXXXXX)"

cleanup() {
  rm -f "$BENCH_BIN" "$LFE_BIN"
}
trap cleanup EXIT

( cd "$REPO" && go build -o "$BENCH_BIN" ./cmd/bench/ )
( cd "$REPO" && GOOS=linux go build -o "$LFE_BIN" ./cmd/live-fork-egress-smoke/ )
echo "  built: cmd/bench -> ${BENCH_BIN}"
echo "  built: cmd/live-fork-egress-smoke -> ${LFE_BIN}"
echo

# ---- percentile helper (nearest-rank, shared by all arms) ----

# sorted_samples holds the newline-separated sorted list of raw samples for
# the current arm. Set it before calling nth() or print_stats().
sorted_samples=""

nth() {
  local p="$1"
  local n
  n="$(printf '%s\n' "$sorted_samples" | wc -l | tr -d ' ')"
  local rank
  rank=$(awk -v p="$p" -v n="$n" 'BEGIN {
    r = (p/100.0)*n; ri = int(r)
    if (r > ri) ri = ri + 1
    if (ri < 1) ri = 1
    print ri
  }')
  printf '%s\n' "$sorted_samples" | sed -n "${rank}p"
}

print_stats() {
  local label="$1"
  local n
  n="$(printf '%s\n' "$sorted_samples" | wc -l | tr -d ' ')"
  local min max p50 p95
  min="$(printf '%s\n' "$sorted_samples" | head -1)"
  max="$(printf '%s\n' "$sorted_samples" | tail -1)"
  p50="$(nth 50)"
  p95="$(nth 95)"
  echo "${label} (ms), N=${n}:"
  echo "  min  ${min}"
  echo "  P50  ${p50}"
  echo "  P95  ${p95}"
  echo "  max  ${max}"
  echo "raw samples (sorted): $(printf '%s ' "$sorted_samples")"
}

# ========================================================================
# ARM 1: cold-fork baseline (non-networked, cmd/bench --mode fork-exec)
#
# Measures: fork start -> first successful Control.Ping over gRPC.
# This is the established isolated engine baseline. It does NOT include
# per-fork networking; it is the reference that bounds the live-fork delta.
# ========================================================================

echo "=== ARM 1: cold-fork baseline (non-networked) ==="
echo "  cmd/bench --mode fork-exec: fork -> first gRPC ping"
echo

BENCH_TEMPLATE="nlf-bench-tpl"
BENCH_JSON="$(mktemp -t bench-fork-exec.XXXXXX.json)"
trap 'rm -f "$BENCH_BIN" "$LFE_BIN" "$BENCH_JSON"' EXIT

"$BENCH_BIN" \
  --mode fork-exec \
  --template "$BENCH_TEMPLATE" \
  --data-dir "$DATA_DIR" \
  --firecracker "$FIRECRACKER" \
  --kernel "$KERNEL" \
  --iterations "$ITERATIONS" \
  --warmup "$WARMUP" \
  --summary \
  --json "$BENCH_JSON"

echo
echo "  (raw JSON in ${BENCH_JSON}; record in bench/results/ with hardware context)"
echo

# ========================================================================
# ARM 2: networked live-fork end-to-end (upper bound)
#
# Measures: wall clock of a complete cmd/live-fork-egress-smoke run, which
# drives engine.ForkRunning on a live networked source through the egress
# proxy (--egress-proxy --proxy-sentinel --proxy-port). Each iteration is
# an independent run of the binary: it creates its own template from --image,
# forks a networked source, holds a keep-alive tunnel through the proxy,
# live-forks the source via ForkRunning, delivers the fork-correctness +
# network handshake to the child, and asserts independent egress on a fresh
# upstream connection at the host stub.
#
# The measured wall clock is an UPPER BOUND on the isolated ForkRunning
# latency. It includes: template creation (boot + snapshot), source cold-fork
# + network setup, egress proxy start, keep-alive tunnel, ForkRunning call,
# child handshake (MAC/IP/reseed/clock-step), and egress assertions. An
# isolated ForkRunning measurement requires extending cmd/bench with a
# live-fork-networked mode that retains the template and source across
# iterations (issue #336).
#
# This arm requires root (or CAP_NET_ADMIN + CAP_NET_RAW) for tap creation
# and nftables rules.
# ========================================================================

echo "=== ARM 2: networked live-fork end-to-end (upper bound) ==="
echo "  cmd/live-fork-egress-smoke: template create + source fork + ForkRunning + assertions"
echo "  flags: --proxy-sentinel ${PROXY_SENTINEL} --proxy-port ${PROXY_PORT}"
echo "  NOTE: timing includes template and source setup overhead (see header for details)"
echo

lfe_samples=()

for i in $(seq 1 $(( WARMUP + ITERATIONS )) ); do
  # unique data dir per iteration so templates never collide across runs.
  ITER_DATA="${DATA_DIR}/lfe-iter-${i}-$$"
  mkdir -p "$ITER_DATA"

  start_ns=$(date +%s%N)

  if "$LFE_BIN" \
      --image "$IMAGE" \
      --data-dir "$ITER_DATA" \
      --firecracker "$FIRECRACKER" \
      --kernel "$KERNEL" \
      --agent-bin "$AGENT_BIN" \
      --proxy-sentinel "$PROXY_SENTINEL" \
      --proxy-port "$PROXY_PORT" \
      >/dev/null 2>&1; then
    end_ns=$(date +%s%N)
    elapsed_ms=$(awk -v s="$start_ns" -v e="$end_ns" \
      'BEGIN { printf "%.1f", (e - s) / 1000000.0 }')
    if [ "$i" -le "$WARMUP" ]; then
      echo "  warmup $i: ${elapsed_ms} ms (discarded)"
    else
      echo "  iteration $((i - WARMUP)): ${elapsed_ms} ms"
      lfe_samples+=("$elapsed_ms")
    fi
  else
    rc=$?
    if [ "$rc" -eq 2 ]; then
      echo "  iteration $i: SETUP ERROR (exit 2) - check /dev/kvm, tap support, nftables" >&2
    else
      echo "  iteration $i: ASSERTION FAILURE (exit ${rc}) - live-fork-egress-smoke check failed" >&2
    fi
  fi

  rm -rf "$ITER_DATA"
done

n_lfe="${#lfe_samples[@]}"
if [ "$n_lfe" -eq 0 ]; then
  echo "no successful live-fork samples collected" >&2
  exit 1
fi

sorted_samples=$(printf '%s\n' "${lfe_samples[@]}" | sort -n)
echo
print_stats "networked live-fork end-to-end latency (upper bound)"
echo

# ========================================================================
# ARM 3: N-way COW fan-out baseline (cold-fork, non-networked)
#
# Measures: for each fan-out width N in --fanout-n, fork ONE warmed template
# snapshot into N children and record (a) each child's fork -> first gRPC
# ping (time-to-ready), and (b) the wall clock from fan-out start to the
# instant the LAST child is ready (wall-clock-to-N-ready). Driven by
# cmd/bench --mode fork-fanout, which is the established engine-level fan-out
# measurement.
#
# This arm does NOT include per-fork networking; it is the structural
# baseline for the networked live-fork fan-out claim. A networked live-fork
# fan-out (N children from one live networked source via concurrent
# ForkRunning calls) requires extending cmd/bench (issue #336).
# ========================================================================

echo "=== ARM 3: N-way COW fan-out baseline (cold-fork, non-networked) ==="
echo "  cmd/bench --mode fork-fanout: widths = ${FANOUT_N}"
echo

FANOUT_JSON="$(mktemp -t bench-fanout.XXXXXX.json)"
trap 'rm -f "$BENCH_BIN" "$LFE_BIN" "$BENCH_JSON" "$FANOUT_JSON"' EXIT

"$BENCH_BIN" \
  --mode fork-fanout \
  --template "$BENCH_TEMPLATE" \
  --data-dir "$DATA_DIR" \
  --firecracker "$FIRECRACKER" \
  --kernel "$KERNEL" \
  --fanout-n "$FANOUT_N" \
  --summary \
  --json "$FANOUT_JSON"

echo
echo "  (raw JSON in ${FANOUT_JSON}; record in bench/results/ with hardware context)"
echo

# Parse the fan-out JSON and print a summary for each width.
python3 - "$FANOUT_JSON" <<'PY'
import json, sys

def nth(samples, p):
    n = len(samples)
    rank = int((p / 100.0) * n)
    if p / 100.0 * n > rank:
        rank += 1
    if rank < 1:
        rank = 1
    return samples[rank - 1]

with open(sys.argv[1]) as f:
    results = json.load(f)

for r in results:
    n = r["n"]
    fo = r["fanout"]
    wall_ms = fo["wall_clock_to_ready_ns"] / 1e6
    raws = sorted(ns / 1e6 for ns in fo.get("raw_time_to_ready_ns", []))
    if not raws:
        print(f"  N={n}: wall-clock-to-N-ready={wall_ms:.1f} ms (no per-child raw samples)")
        continue
    mn   = raws[0]
    mx   = raws[-1]
    p50  = nth(raws, 50)
    p95  = nth(raws, 95)
    print(f"  N={n}: wall-clock-to-N-ready={wall_ms:.1f} ms | per-child min={mn:.1f} P50={p50:.1f} P95={p95:.1f} max={mx:.1f} ms")
PY

echo
echo "Numbers are produced by the real KVM-backed engine on THIS host."
echo "Record in bench/results/ with the host spec, Firecracker version, kernel, and rootfs contents."
echo "No expected or reference latency figures are stated here (CLAUDE.md operating principle 1)."
