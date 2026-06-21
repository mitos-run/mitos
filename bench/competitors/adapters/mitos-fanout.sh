#!/usr/bin/env bash
#
# mitos-fanout.sh -- the reference fan-out adapter, wired to THIS repo's own
# cmd/bench fork-fanout mode.
#
# This adapter measures mitos's 1-to-N live copy-on-write fan-out: fork ONE
# warmed base (the template snapshot, built with the repo loaded and deps
# installed) into N children and report the wall clock until all N are ready
# plus each child's time-to-ready. It is the only fan-out adapter here wired to a
# real system, because mitos is the system this repo owns. The competitor
# adapters are placeholders a reproducer fills in for their own deployment.
#
# It drives the real KVM-backed engine in-process via cmd/bench (no forkd, no
# gRPC, no HTTP in the path), exactly like bench/README.md describes for the
# other modes. On a host WITHOUT /dev/kvm the engine fails at construction and
# cmd/bench exits non-zero, so this adapter can never emit a fabricated off-KVM
# number.
#
# "Warm" for mitos: a pre-built, verified template snapshot under --data-dir with
# the repo and deps already baked in, so every child is a live COW fork of that
# warmed state, not a cold boot. Document the template contents alongside the
# result.
#
# Required environment:
#   MITOS_TEMPLATE     template (snapshot) id to fork from
#   MITOS_DATA_DIR     data dir holding the template (default: /var/lib/mitos)
#   MITOS_FIRECRACKER  firecracker binary (default: /usr/local/bin/firecracker)
#   MITOS_KERNEL       guest kernel (default: $MITOS_DATA_DIR/vmlinux)
#
# Sourced by run-fanout-comparison.sh; defines warm() and fanout() only.

: "${MITOS_DATA_DIR:=/var/lib/mitos}"
: "${MITOS_FIRECRACKER:=/usr/local/bin/firecracker}"

_mitos_fanout_bin=""

warm() {
  if [ -z "${MITOS_TEMPLATE:-}" ]; then
    echo "mitos-fanout adapter needs MITOS_TEMPLATE set" >&2
    return 1
  fi
  command -v go >/dev/null 2>&1 || { echo "go not found on PATH" >&2; return 1; }
  command -v python3 >/dev/null 2>&1 || { echo "python3 not found on PATH (used to parse bench JSON)" >&2; return 1; }
  local repo_root
  repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
  _mitos_fanout_bin="$(mktemp -t fanout-bench.XXXXXX)"
  ( cd "$repo_root" && go build -o "$_mitos_fanout_bin" ./cmd/bench/ )
  echo "mitos-fanout adapter warmed: harness built, template=$MITOS_TEMPLATE assumed pre-built and verified" >&2
}

fanout() {
  local n="$1"
  : "${MITOS_KERNEL:=$MITOS_DATA_DIR/vmlinux}"
  local json
  json="$(mktemp -t fanout-json.XXXXXX)"
  # One fan-out at width N. cmd/bench forks one base into N children, measures
  # each child's fork->first-exec on a shared wall clock, and writes the
  # aggregated FanOutResult (including raw per-child samples) as JSON.
  "$_mitos_fanout_bin" \
    --mode fork-fanout \
    --template "$MITOS_TEMPLATE" \
    --data-dir "$MITOS_DATA_DIR" \
    --firecracker "$MITOS_FIRECRACKER" \
    --kernel "$MITOS_KERNEL" \
    --fanout-n "$n" \
    --json "$json" >/dev/null 2>&1 || { rm -f "$json"; return 1; }

  # Emit line 1 = wall-clock-to-N-ready ms, then one line per raw child sample
  # (ms). The driver re-derives the per-child distribution by its own method.
  python3 - "$json" <<'PY' || { rm -f "$json"; return 1; }
import json, sys
with open(sys.argv[1]) as f:
    results = json.load(f)
if not results:
    sys.exit(1)
fo = results[0]["fanout"]
print(fo["wall_clock_to_ready_ns"] / 1e6)
for ns in fo.get("raw_time_to_ready_ns", []):
    print(ns / 1e6)
PY
  local rc=$?
  rm -f "$json"
  return $rc
}
