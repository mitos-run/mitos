#!/usr/bin/env bash
#
# agent-sandbox-kata.sh -- placeholder adapter for Agent Sandbox + Kata.
#
# This is a SCAFFOLD, not a runnable measurement. To produce an Agent Sandbox +
# Kata number this repo can publish as OUR measurement, a reproducer must:
#
#   1. Deploy the upstream Agent Sandbox controller with the Kata runtime class
#      on the SAME hardware as the mitos run (issue #16 reference node).
#   2. Pre-pull the sandbox image and make the Kata runtime ready, so the number
#      is the intended hot path (document what "warm" means; Agent Sandbox cold-
#      creates a pod on each sandbox, so "warm" here is image + runtime ready).
#   3. Implement create_exec() below to: create one Agent Sandbox Sandbox, wait
#      for its pod to serve, exec one trivial command, tear it down, and print
#      the create->first-exec milliseconds for that one iteration.
#
# Until then this adapter exits non-zero so run-comparison.sh can NEVER emit a
# fabricated Agent Sandbox + Kata number. Any figure quoted before this is filled
# in is VENDOR-PUBLISHED (cite the project's own source), not our measurement.
#
# Sourced by run-comparison.sh; defines warm() and create_exec() only.

warm() {
  echo "agent-sandbox-kata adapter is a scaffold: deploy Agent Sandbox + Kata and implement warm()" >&2
}

create_exec() {
  echo "agent-sandbox-kata adapter is a scaffold: implement create_exec() against your deployment" >&2
  echo "until then no Agent Sandbox + Kata number is OUR measurement; any quoted figure is vendor-published" >&2
  return 1
}
