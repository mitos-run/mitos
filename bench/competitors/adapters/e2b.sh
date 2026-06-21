#!/usr/bin/env bash
#
# e2b.sh -- placeholder adapter for E2B (self-hosted).
#
# This is a SCAFFOLD, not a runnable measurement. To produce an E2B number that
# this repo can publish as OUR measurement, a reproducer must:
#
#   1. Deploy the open-source E2B infrastructure stack on the SAME hardware used
#      for the mitos run (issue #16 reference node).
#   2. Pre-build the E2B template and warm any sandbox pool E2B supports, so the
#      measured number is E2B's intended hot path (document what "warm" means).
#   3. Implement create_exec() below to: create one E2B sandbox via the E2B SDK
#      or API, exec one trivial command, tear it down, and print the
#      create->first-exec milliseconds for that one iteration.
#
# Until that is done, this adapter exits non-zero so run-comparison.sh can NEVER
# emit a fabricated E2B number. Any E2B figure quoted before this is filled in is
# VENDOR-PUBLISHED (cite E2B's own source), not our measurement.
#
# Sourced by run-comparison.sh; defines warm() and create_exec() only.

warm() {
  echo "e2b adapter is a scaffold: deploy self-hosted E2B and implement warm()" >&2
}

create_exec() {
  echo "e2b adapter is a scaffold: implement create_exec() against your self-hosted E2B" >&2
  echo "until then no E2B number is OUR measurement; any quoted figure is vendor-published" >&2
  return 1
}
