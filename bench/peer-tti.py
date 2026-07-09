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

Usage:
    python3 bench/peer-tti.py [date]      # date like 2026-07-09, default latest
"""
import json
import math
import sys
import urllib.request

# The repo's default branch is master, not main.
RAW = ("https://raw.githubusercontent.com/computesdk/benchmarks/master/"
       "results/sequential_tti/%s.json")


def nearest_rank(values, pct):
    values = sorted(values)
    return values[max(1, math.ceil(pct / 100.0 * len(values))) - 1]


def main():
    which = sys.argv[1] if len(sys.argv) > 1 else "latest"
    with urllib.request.urlopen(RAW % which, timeout=30) as r:
        data = json.load(r)

    print("run: %s  config: %s" % (data["timestamp"], data["config"]))
    rows = []
    for result in data["results"]:
        tti = [i["ttiMs"] for i in result.get("iterations", [])
               if isinstance(i.get("ttiMs"), (int, float)) and not i.get("error")]
        if not tti:
            continue
        rows.append((result["provider"], nearest_rank(tti, 50),
                     nearest_rank(tti, 90), min(tti), len(tti)))

    rows.sort(key=lambda r: r[1])
    print("%-16s%9s%9s%9s%6s" % ("provider", "P50", "P90", "min", "n"))
    for provider, p50, p90, low, n in rows:
        print("%-16s%9.1f%9.1f%9.1f%6d" % (provider, p50, p90, low, n))
    return 0


if __name__ == "__main__":
    sys.exit(main())
