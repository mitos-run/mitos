#!/usr/bin/env bash
#
# run-comparison.sh
#
# Neutral driver for the competitor comparison (issue #15 item 5). It runs the
# SAME create-sandbox -> first-exec method against any system via an adapter, so
# every system is measured identically. It does NOT invent numbers: it only runs
# the adapter the maintainer points it at, on the maintainer's hardware.
#
# Adapter contract: an adapter is a shell script that, when sourced, defines two
# functions:
#   warm          -- bring the system to its intended steady (warm) state; run once.
#   create_exec   -- create one fresh sandbox, exec one trivial command in it, and
#                    print ONE number to stdout: the create -> first-exec
#                    milliseconds for this iteration. Exit non-zero on failure.
# See adapters/template.sh for the documented contract and adapters/mitos.sh for
# the reference (wired to this repo's own harness). The competitor adapters are
# placeholders that exit non-zero until a reproducer fills them in, so a run can
# never silently emit a fabricated competitor number.
#
# Usage:
#   bench/competitors/run-comparison.sh <adapter.sh> [iterations] [warmup]
#
#   <adapter.sh>   path to an adapter (for example adapters/mitos.sh)
#   [iterations]   measured iterations (default: 20)
#   [warmup]       discarded warmup iterations (default: 3)
#
# It prints min / P50 / P90 / P99 / max (nearest-rank) and the raw samples.
# Record the output in bench/results/ with the host spec and the system version.
#
set -euo pipefail

if [ "$#" -lt 1 ]; then
  echo "usage: $0 <adapter.sh> [iterations] [warmup]" >&2
  exit 2
fi

ADAPTER="$1"
ITERATIONS="${2:-20}"
WARMUP="${3:-3}"

if [ ! -f "$ADAPTER" ]; then
  echo "adapter $ADAPTER not found" >&2
  exit 1
fi

# shellcheck disable=SC1090
source "$ADAPTER"

if ! declare -F warm >/dev/null || ! declare -F create_exec >/dev/null; then
  echo "adapter $ADAPTER must define warm() and create_exec()" >&2
  exit 1
fi

echo "comparison: adapter=$ADAPTER iterations=$ITERATIONS warmup=$WARMUP"
echo "warming system ..."
warm

echo "discarding $WARMUP warmup iteration(s) ..."
for _ in $(seq 1 "$WARMUP"); do
  create_exec >/dev/null
done

samples=()
for i in $(seq 1 "$ITERATIONS"); do
  v="$(create_exec)"
  echo "  iter $i: ${v} ms"
  samples+=("$v")
done

n="${#samples[@]}"
if [ "$n" -eq 0 ]; then
  echo "no samples collected" >&2
  exit 1
fi

sorted=$(printf '%s\n' "${samples[@]}" | sort -n)
nth() {
  local p="$1" rank
  rank=$(awk -v p="$p" -v n="$n" 'BEGIN { r = (p/100.0)*n; ri = int(r); if (r > ri) ri = ri + 1; if (ri < 1) ri = 1; print ri }')
  printf '%s\n' "$sorted" | sed -n "${rank}p"
}

echo
echo "create -> first-exec (ms), N=$n:"
echo "  min  $(printf '%s\n' "$sorted" | head -1)"
echo "  P50  $(nth 50)"
echo "  P90  $(nth 90)"
echo "  P99  $(nth 99)"
echo "  max  $(printf '%s\n' "$sorted" | tail -1)"
echo
echo "raw samples (sorted): $(printf '%s ' $sorted)"
echo
echo "Record this with the host (CPU, kernel, system version, image/size) in"
echo "bench/results/. A competitor figure is OUR measurement only when produced"
echo "by this run on the same hardware; otherwise it stays vendor-published."
