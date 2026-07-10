#!/usr/bin/env python3
"""Recompute the peer Time-to-Interactive table from ComputeSDK's raw data.

The vendor leaderboard at computesdk.com renders client side, and the README shows
only chart placeholders, so the numbers there cannot be checked by reading either.
This fetches the raw per-iteration JSON the harness commits and computes the
percentiles itself, which is the only form of the number this repo is allowed to
publish (see the no-unverified-claims rule in CLAUDE.md).

It measures THEIR harness: `node -v`, 100 sequential un-paced iterations, from their
runner. Our own row comes from bench/tti-latency.py and is not comparable without the
caveats recorded in bench/results/2026-07-10-tti-hosted.md.

REPRODUCIBILITY: `latest.json` on the default branch is MUTABLE. A number published
from it cannot be re-derived later. Pass --ref with the commit SHA that produced a
published table, and record that SHA next to the number.

Usage:
    python3 bench/peer-tti.py                       # today's latest, on master
    python3 bench/peer-tti.py --date 2026-07-09     # that day's frozen file
    python3 bench/peer-tti.py --ref <commit-sha>    # pinned, reproducible
"""
import argparse
import json
import math
import sys
import urllib.request

# The repo's default branch is master, not main.
RAW = ("https://raw.githubusercontent.com/computesdk/benchmarks/%s/"
       "results/sequential_tti/%s.json")


def is_valid_sample(value):
    """A TTI sample is a finite, non-negative real number.

    bool is a subclass of int in Python, and JSON null/NaN/Infinity all survive
    json.loads, so an unguarded isinstance check would admit True, NaN, and -1 as
    latencies and silently corrupt every percentile below.
    """
    if isinstance(value, bool) or not isinstance(value, (int, float)):
        return False
    return math.isfinite(value) and value >= 0


def nearest_rank(values, pct):
    values = sorted(values)
    return values[max(1, math.ceil(pct / 100.0 * len(values))) - 1]


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--date", default="latest",
                    help="a frozen daily file (e.g. 2026-07-09), or 'latest'")
    ap.add_argument("--ref", default="master",
                    help="git ref of computesdk/benchmarks to read; pass a commit SHA "
                         "to make a published table reproducible")
    args = ap.parse_args()

    url = RAW % (args.ref, args.date)
    with urllib.request.urlopen(url, timeout=30) as r:
        data = json.load(r)

    print("source: %s" % url)
    print("run: %s  config: %s" % (data["timestamp"], data["config"]))
    if args.ref == "master" and args.date == "latest":
        print("WARNING: master/latest.json is mutable; pin --ref <sha> for a published number",
              file=sys.stderr)

    rows = []
    for result in data["results"]:
        iterations = result.get("iterations", [])
        tti = [i["ttiMs"] for i in iterations
               if not i.get("error") and is_valid_sample(i.get("ttiMs"))]
        # NEVER drop a provider that failed every iteration: doing so would make the
        # leaderboard look better than the run it came from.
        rows.append((result["provider"], tti, len(iterations) - len(tti)))

    # Providers with no valid sample sort last, and say so rather than vanishing.
    rows.sort(key=lambda r: (not r[1], nearest_rank(r[1], 50) if r[1] else 0))

    print("%-16s%9s%9s%9s%6s%7s" % ("provider", "P50", "P90", "min", "n", "err"))
    for provider, tti, errors in rows:
        if not tti:
            print("%-16s%9s%9s%9s%6d%7d" % (provider, "-", "-", "-", 0, errors))
            continue
        print("%-16s%9.1f%9.1f%9.1f%6d%7d"
              % (provider, nearest_rank(tti, 50), nearest_rank(tti, 90),
                 min(tti), len(tti), errors))
    return 0


if __name__ == "__main__":
    sys.exit(main())
