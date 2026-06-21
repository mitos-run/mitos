#!/usr/bin/env bash
#
# run-fanout-comparison.sh
#
# Neutral driver for the 1-to-N fan-out competitor comparison (issue #207). It
# runs the SAME fan-out shape against any system via an adapter: take ONE warmed
# base (repo loaded, deps installed) and bring up N children from it, then
# report the wall clock until all N children are ready and the per-child
# time-to-ready distribution. Every system is measured by the same method so the
# comparison is apples-to-apples.
#
# This is the contested-claim harness. The defensible mitos claim is sub-second
# 1-to-N live copy-on-write fan-out: forking one warmed base into many children
# cheaply. Modal markets a snapshot/branch capability and Daytona/E2B start cold
# sandboxes; the only honest way to compare is to measure the SAME fan-out shape
# on each and record whether mitos fork actually beats branching, or whether the
# wedge is self-hosting plus per-fork network isolation rather than raw speed.
# This driver does NOT decide that. It only runs the adapter the maintainer
# points it at, on the maintainer's hardware, and never invents a number.
#
# Adapter contract: an adapter is a shell script that, when sourced, defines:
#   warm           -- bring the system to its intended steady (warm) state; run once.
#   fanout <N>     -- create ONE base and bring up N children from it, then print:
#                       line 1:  the wall-clock-to-N-ready MILLISECONDS (one number)
#                       lines 2..N+1: each child's time-to-ready MILLISECONDS (one
#                                     number per line)
#                     Exit non-zero on any failure so the driver records no bogus
#                     sample. See adapters/template-fanout.sh for the contract and
#                     adapters/mitos-fanout.sh for the reference (wired to this
#                     repo's own cmd/bench fork-fanout mode). The competitor
#                     adapters exit non-zero until a reproducer fills them in, so a
#                     run can never silently emit a fabricated competitor number.
#
# Usage:
#   bench/competitors/run-fanout-comparison.sh <adapter.sh> [n1,n2,...]
#
#   <adapter.sh>   path to a fan-out adapter (for example adapters/mitos-fanout.sh)
#   [n1,n2,...]    comma-separated fan-out widths (default: 1,4,16,64)
#
# For each N it prints the wall-clock-to-N-ready and the per-child min / P50 /
# P90 / max (nearest-rank). Record the output in bench/results/ with the host
# spec and the system version. A competitor figure is OUR measurement only when
# produced by this run on the same hardware; otherwise it stays vendor-published.
#
set -euo pipefail

if [ "$#" -lt 1 ]; then
  echo "usage: $0 <adapter.sh> [n1,n2,...]" >&2
  exit 2
fi

ADAPTER="$1"
NS_CSV="${2:-1,4,16,64}"

if [ ! -f "$ADAPTER" ]; then
  echo "adapter $ADAPTER not found" >&2
  exit 1
fi

# shellcheck disable=SC1090
source "$ADAPTER"

if ! declare -F warm >/dev/null || ! declare -F fanout >/dev/null; then
  echo "adapter $ADAPTER must define warm() and fanout()" >&2
  exit 1
fi

echo "fan-out comparison: adapter=$ADAPTER widths=$NS_CSV"
echo "warming system ..."
warm

IFS=',' read -r -a NS <<<"$NS_CSV"

for N in "${NS[@]}"; do
  if ! [[ "$N" =~ ^[0-9]+$ ]] || [ "$N" -lt 1 ]; then
    echo "invalid fan-out width: $N" >&2
    exit 1
  fi

  echo
  echo "=== fan-out N=$N ==="
  mapfile -t lines < <(fanout "$N")

  if [ "${#lines[@]}" -lt 2 ]; then
    echo "adapter returned fewer than 2 lines for N=$N (need wall-clock + per-child)" >&2
    exit 1
  fi

  wall="${lines[0]}"
  per_child=("${lines[@]:1}")

  echo "wall-clock-to-${N}-ready: ${wall} ms"

  cn="${#per_child[@]}"
  sorted=$(printf '%s\n' "${per_child[@]}" | sort -n)
  nth() {
    local p="$1" rank
    rank=$(awk -v p="$p" -v n="$cn" 'BEGIN { r = (p/100.0)*n; ri = int(r); if (r > ri) ri = ri + 1; if (ri < 1) ri = 1; print ri }')
    printf '%s\n' "$sorted" | sed -n "${rank}p"
  }
  echo "per-child time-to-ready (ms), children=$cn:"
  echo "  min  $(printf '%s\n' "$sorted" | head -1)"
  echo "  P50  $(nth 50)"
  echo "  P90  $(nth 90)"
  echo "  max  $(printf '%s\n' "$sorted" | tail -1)"
done

echo
echo "Record this with the host (CPU, kernel, system version, base image/size,"
echo "and what warm state was pre-established) in bench/results/. A competitor"
echo "figure is OUR measurement only when produced by this run on the same"
echo "hardware; otherwise it stays vendor-published."
