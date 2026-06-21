#!/usr/bin/env bash
#
# modal-fanout.sh -- placeholder fan-out adapter for Modal Sandboxes
# snapshot/fork.
#
# This is a SCAFFOLD, not a runnable measurement. Modal markets a sandbox
# snapshot and branch/fork capability, which is the closest competitor to mitos's
# 1-to-N live copy-on-write fan-out, so it is the headline comparison for issue
# #207. To produce a Modal number this repo can publish as OUR measurement, a
# reproducer must:
#
#   1. Stand up a Modal account/workspace and build ONE warmed Modal sandbox
#      snapshot with the repo loaded and deps installed (document what "warm"
#      means: the snapshot/memory snapshot Modal restores from).
#   2. Implement fanout(N) below to: from that ONE snapshot, branch/fork N child
#      sandboxes (Modal's snapshot-restore or branch API), wait until each child
#      serves a first exec, tear them down, and print:
#        line 1:        wall-clock-to-N-ready MILLISECONDS (one number)
#        lines 2..N+1:  each child's time-to-ready MILLISECONDS (one per line)
#
# Modal is NOT self-hostable, so this comparison is necessarily run against
# Modal's hosted service, not the same reference hardware as mitos. Record that
# asymmetry explicitly with any Modal number: it measures Modal's hosted fan-out,
# and the mitos number measures self-hosted fan-out on the reference node. Until
# this adapter is filled in, any Modal figure is VENDOR-PUBLISHED (cite Modal's
# own source), not our measurement.
#
# This adapter exits non-zero so run-fanout-comparison.sh can NEVER emit a
# fabricated Modal number.
#
# Sourced by run-fanout-comparison.sh; defines warm() and fanout() only.

warm() {
  echo "modal-fanout adapter is a scaffold: build a warmed Modal snapshot and implement warm()" >&2
}

fanout() {
  echo "modal-fanout adapter is a scaffold: implement fanout(N) to branch one Modal snapshot into N children" >&2
  echo "until then no Modal number is OUR measurement; any quoted figure is vendor-published" >&2
  return 1
}
