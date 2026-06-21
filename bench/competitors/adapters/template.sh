#!/usr/bin/env bash
#
# template.sh -- the adapter contract for run-comparison.sh.
#
# Copy this to a new adapter and implement the two functions for your system.
# An adapter is SOURCED by run-comparison.sh, so it must only DEFINE functions
# (no top-level side effects, no `set -e` that would leak into the driver).
#
# Contract:
#
#   warm()
#     Bring the system to its intended steady (warm) state: warm pool filled,
#     image pre-pulled, template snapshot pre-built, runtime class ready, etc.
#     Run once before measurement. Document here exactly what "warm" means for
#     this system so the comparison is honest about hot-path vs cold-pull.
#
#   create_exec()
#     Create ONE fresh sandbox, exec ONE trivial command inside it, tear it
#     down, and print exactly ONE number to stdout: the create -> first-exec
#     wall-clock MILLISECONDS for this single iteration. Return non-zero on any
#     failure so the driver stops rather than recording a bogus sample.
#
# This template itself is not runnable: create_exec exits non-zero so an
# unfilled adapter can never emit a fabricated number.

warm() {
  echo "template adapter: implement warm() for your system" >&2
}

create_exec() {
  echo "template adapter: implement create_exec() to print create->first-exec ms" >&2
  return 1
}
