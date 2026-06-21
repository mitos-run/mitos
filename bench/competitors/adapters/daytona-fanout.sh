#!/usr/bin/env bash
#
# daytona-fanout.sh -- placeholder fan-out adapter for Daytona (OSS).
#
# This is a SCAFFOLD, not a runnable measurement. Daytona starts cold workspaces
# rather than forking one warmed base, so its "fan-out" is N independent creates
# from the same pre-pulled image. Measuring that SAME shape is exactly the honest
# comparison issue #207 asks for: it shows the cold-start baseline against
# mitos's live copy-on-write fan-out. To produce a Daytona number this repo can
# publish as OUR measurement, a reproducer must:
#
#   1. Stand up a self-hosted Daytona instance on the SAME hardware as the mitos
#      run (issue #16 reference node).
#   2. Pre-pull the workspace image and warm any pool Daytona supports (document
#      what "warm" means; Daytona cold-creates a workspace per sandbox).
#   3. Implement fanout(N) below to: create N Daytona workspaces from the same
#      warmed image (the closest thing to fan-out Daytona offers), wait until
#      each serves a first exec, tear them down, and print:
#        line 1:        wall-clock-to-N-ready MILLISECONDS (one number)
#        lines 2..N+1:  each workspace's time-to-ready MILLISECONDS (one per line)
#
# Until then this adapter exits non-zero so run-fanout-comparison.sh can NEVER
# emit a fabricated Daytona number. Any Daytona figure quoted before this is
# filled in is VENDOR-PUBLISHED (cite Daytona's own source), not our measurement.
#
# Sourced by run-fanout-comparison.sh; defines warm() and fanout() only.

warm() {
  echo "daytona-fanout adapter is a scaffold: deploy self-hosted Daytona and implement warm()" >&2
}

fanout() {
  echo "daytona-fanout adapter is a scaffold: implement fanout(N) to create N Daytona workspaces from one image" >&2
  echo "until then no Daytona number is OUR measurement; any quoted figure is vendor-published" >&2
  return 1
}
