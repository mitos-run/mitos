#!/usr/bin/env bash
#
# template-fanout.sh -- the adapter contract for run-fanout-comparison.sh.
#
# Copy this to a new fan-out adapter and implement the two functions for your
# system. An adapter is SOURCED by run-fanout-comparison.sh, so it must only
# DEFINE functions (no top-level side effects, no `set -e` that would leak into
# the driver).
#
# Contract:
#
#   warm()
#     Bring the system to its intended steady (warm) state: warm pool filled,
#     base image pre-pulled, template snapshot pre-built with the repo loaded and
#     deps installed, runtime class ready, etc. Run once before measurement.
#     Document here exactly what "warm" means for this system so the comparison
#     is honest about a designed fan-out hot path vs a cold cluster.
#
#   fanout(N)
#     Take ONE warmed base and bring up N children from it (a fork, a branch, or
#     N cold creates from the same image, whatever the system's fan-out is), wait
#     until every child is ready (first successful exec inside it), tear them all
#     down, and print:
#       line 1:        the wall-clock-to-N-ready MILLISECONDS (one number): the
#                      wall clock from the fan-out start to the instant the LAST
#                      child is ready.
#       lines 2..N+1:  each child's time-to-ready MILLISECONDS, one number per
#                      line (N lines).
#     Return non-zero on any failure so the driver records no bogus sample.
#
# This template itself is not runnable: fanout() exits non-zero so an unfilled
# adapter can never emit a fabricated number.

warm() {
  echo "template-fanout adapter: implement warm() for your system" >&2
}

fanout() {
  echo "template-fanout adapter: implement fanout(N) to print wall-clock then per-child ms" >&2
  return 1
}
