#!/usr/bin/env python3
"""Time to Interactive (TTI): API request -> first successful exec inside the sandbox.

This is the only number that is apples-to-apples with the public ComputeSDK
benchmark (github.com/computesdk/benchmarks), which every hosted sandbox vendor is
measured against: the clock starts when the client calls create() and stops when a
command has actually RUN INSIDE the sandbox and returned. It therefore includes the
client round trip, auth, the control-plane reconcile, the microVM restore, and the
first exec.

It is deliberately NOT the same thing as `bench/husk-activate-latency.sh`, which
measures only the engine's warm-claim activate (the snapshot restore plus the
fork-correctness handshake) and excludes everything around it. Quoting the engine
number against a competitor's create-API number is the category error this harness
exists to avoid.

Run it from a client in the SAME REGION as the API. Measuring from a laptop across
the public internet mostly measures the internet: a 404 on an unrouted path costs
250 ms or more from a random location and 6 ms from inside the cluster.

Iterations are PACED (default 13 s apart) so a free-tier key stays under the hosted
creation rate limit (5 creates/minute, quota.TierFree.CreationRatePerMinute): this
harness measures per-create latency, not throughput, and a rate-limited iteration
would otherwise be recorded as a failure of the engine rather than of the pacing. A
rate_limited response triggers a backoff and a retry of that iteration, counted
separately, so throttling is visible but never pollutes the latency distribution.

Usage:
    MITOS_API_KEY=... python3 bench/tti-latency.py [iterations] [--template python]

Reports min / P50 / P90 / P95 / P99 / max over N sequential iterations, plus the
create and first-exec split, and the success rate. Percentiles are nearest-rank, the
same convention bench/husk-activate-latency.sh uses.
"""

import argparse
import json
import os
import statistics
import sys
import time
import urllib.request

sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", "sdk", "python"))
import mitos  # noqa: E402

API = os.environ.get("MITOS_API_BASE", "https://api.mitos.run")

# The probe command. ComputeSDK uses `node -v`; the equivalent smallest possible
# "the sandbox really ran my code and came back" for the python template.
PROBE = "print(1)"


def _req(path, method="GET", key=""):
    r = urllib.request.Request(API + path, method=method,
                               headers={"Authorization": "Bearer " + key})
    return urllib.request.urlopen(r)


def cleanup(key):
    """Terminate every live sandbox for this org (the free tier caps concurrency)."""
    try:
        d = json.load(_req("/v1/sandboxes", key=key))
    except Exception:
        return
    for s in (d.get("sandboxes") or d.get("items") or []):
        try:
            _req("/v1/sandboxes/" + s["id"], method="DELETE", key=key)
        except Exception:
            pass


def nearest_rank(sorted_vals, pct):
    if not sorted_vals:
        return float("nan")
    import math
    rank = max(1, math.ceil(pct / 100.0 * len(sorted_vals)))
    return sorted_vals[rank - 1]


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("iterations", nargs="?", type=int, default=20)
    ap.add_argument("--template", default="python")
    ap.add_argument("--interval", type=float, default=13.0,
                    help="seconds between iterations; keep under the tier creation rate")
    args = ap.parse_args()

    key = os.environ.get("MITOS_API_KEY")
    if not key:
        print("set MITOS_API_KEY", file=sys.stderr)
        return 2

    cleanup(key)
    time.sleep(2)

    tti, creates, execs = [], [], []
    failures = throttled = 0
    for i in range(args.iterations):
        for attempt in range(3):
            sb = None
            try:
                t0 = time.perf_counter()
                sb = mitos.create(args.template, api_key=key)
                t1 = time.perf_counter()
                sb.run_code(PROBE)
                t2 = time.perf_counter()
            except Exception as e:
                if "rate_limited" in str(e):
                    # Pacing failure, not an engine failure: back off past the token
                    # refill and retry this iteration. Counted so it stays visible.
                    throttled += 1
                    print("  iter %2d: rate_limited, backing off" % (i + 1), file=sys.stderr)
                    cleanup(key)
                    time.sleep(30)
                    continue
                failures += 1
                print("  iter %2d: FAILED %s" % (i + 1, str(e)[:80]), file=sys.stderr)
                cleanup(key)
                time.sleep(args.interval)
                break
            finally:
                if sb is not None:
                    try:
                        sb.terminate()
                    except Exception:
                        pass
            creates.append((t1 - t0) * 1000)
            execs.append((t2 - t1) * 1000)
            tti.append((t2 - t0) * 1000)
            print("  iter %2d: TTI %7.1f ms  (create %6.1f + first exec %6.1f)"
                  % (i + 1, tti[-1], creates[-1], execs[-1]))
            time.sleep(args.interval)
            break

    if not tti:
        print("no successful iterations", file=sys.stderr)
        return 1

    s = sorted(tti)
    n = len(s)
    print()
    print("Time to Interactive (create -> first exec returned), ms, N=%d" % n)
    print("  min   %7.1f" % s[0])
    print("  P50   %7.1f" % nearest_rank(s, 50))
    print("  P90   %7.1f" % nearest_rank(s, 90))
    print("  P95   %7.1f" % nearest_rank(s, 95))
    print("  P99   %7.1f" % nearest_rank(s, 99))
    print("  max   %7.1f" % s[-1])
    print("  mean  %7.1f" % statistics.fmean(s))
    print()
    print("  split: create median %.1f ms, first-exec median %.1f ms"
          % (statistics.median(creates), statistics.median(execs)))
    print("  success rate: %d/%d (%.1f%%), rate-limited retries: %d"
          % (n, n + failures, 100.0 * n / (n + failures), throttled))
    return 0


if __name__ == "__main__":
    sys.exit(main())
