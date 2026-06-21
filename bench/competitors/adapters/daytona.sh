#!/usr/bin/env bash
#
# daytona.sh -- placeholder adapter for Daytona (OSS).
#
# This is a SCAFFOLD, not a runnable measurement. To produce a Daytona number
# this repo can publish as OUR measurement, a reproducer must:
#
#   1. Stand up a self-hosted Daytona instance on the SAME hardware as the mitos
#      run (issue #16 reference node).
#   2. Pre-pull the workspace image and warm any pool Daytona supports, so the
#      number is Daytona's intended hot path (document what "warm" means).
#   3. Implement create_exec() below to: create one Daytona workspace/sandbox,
#      exec one trivial command, tear it down, and print the create->first-exec
#      milliseconds for that one iteration.
#
# Until then this adapter exits non-zero so run-comparison.sh can NEVER emit a
# fabricated Daytona number. Any Daytona figure quoted before this is filled in
# is VENDOR-PUBLISHED (cite Daytona's own source), not our measurement.
#
# Sourced by run-comparison.sh; defines warm() and create_exec() only.

warm() {
  echo "daytona adapter is a scaffold: deploy self-hosted Daytona and implement warm()" >&2
}

create_exec() {
  echo "daytona adapter is a scaffold: implement create_exec() against your Daytona instance" >&2
  echo "until then no Daytona number is OUR measurement; any quoted figure is vendor-published" >&2
  return 1
}
