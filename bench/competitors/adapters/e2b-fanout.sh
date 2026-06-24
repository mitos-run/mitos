#!/usr/bin/env bash
#
# e2b-fanout.sh -- placeholder fan-out adapter for E2B (self-hosted).
#
# This is a SCAFFOLD, not a runnable measurement. E2B starts sandboxes from a
# pre-built template rather than forking one warmed base, so its "fan-out" is N
# creates from the same template. Measuring that SAME shape is the honest cold-
# start baseline against Mitos's live copy-on-write fan-out. To produce an E2B
# number this repo can publish as OUR measurement, a reproducer must:
#
#   1. Deploy the open-source E2B infrastructure stack on the SAME hardware as
#      the Mitos run (issue #16 reference node).
#   2. Pre-build the E2B template and warm any sandbox pool E2B supports
#      (document what "warm" means).
#   3. Implement fanout(N) below to: create N E2B sandboxes from the same warmed
#      template, wait until each serves a first exec, tear them down, and print:
#        line 1:        wall-clock-to-N-ready MILLISECONDS (one number)
#        lines 2..N+1:  each sandbox's time-to-ready MILLISECONDS (one per line)
#
# Until then this adapter exits non-zero so run-fanout-comparison.sh can NEVER
# emit a fabricated E2B number. Any E2B figure quoted before this is filled in is
# VENDOR-PUBLISHED (cite E2B's own source), not our measurement.
#
# Sourced by run-fanout-comparison.sh; defines warm() and fanout() only.

warm() {
  echo "e2b-fanout adapter is a scaffold: deploy self-hosted E2B and implement warm()" >&2
}

fanout() {
  echo "e2b-fanout adapter is a scaffold: implement fanout(N) to create N E2B sandboxes from one template" >&2
  echo "until then no E2B number is OUR measurement; any quoted figure is vendor-published" >&2
  return 1
}
