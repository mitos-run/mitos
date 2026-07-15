#!/usr/bin/env python3
"""Verdict for the nightly hosted TTI canary.

Reads a `bench/tti-latency.py` summary (from a file or stdin) and the threshold
file `bench/canary-baseline.json`, then decides pass or regression:

  - TTI P50 must be <= tti_p50_ceiling_ms
  - success rate must be >= min_success_rate_pct

Exit 0 = within thresholds, exit 1 = regression (the workflow then opens the
tracking issue and fails). A parse failure is exit 2 (a canary that cannot read
its own input must be loud, not silently green).

Pure and offline: it does not touch the network. The workflow owns the GitHub
issue side effect so this stays unit-checkable via --self-test.
"""
import argparse
import json
import re
import sys


def parse_summary(text):
    """Extract (tti_p50_ms, success_pct) from a tti-latency.py summary block.

    The harness prints, among other lines:
        P50      96.8
        success rate: 100/100 (100.0%), rate-limited retries: 0
    We match the FIRST 'P50' line (TTI section) and the success percentage.
    """
    p50 = None
    m = re.search(r"^\s*P50\s+([0-9]+\.?[0-9]*)\s*$", text, re.MULTILINE)
    if m:
        p50 = float(m.group(1))
    success = None
    m = re.search(r"success rate:\s*\d+/\d+\s*\(([0-9]+\.?[0-9]*)%\)", text)
    if m:
        success = float(m.group(1))
    return p50, success


def verdict(text, baseline):
    p50, success = parse_summary(text)
    if p50 is None or success is None:
        return 2, f"could not parse TTI P50 (got {p50}) or success rate (got {success}) from the harness output"
    ceiling = baseline["tti_p50_ceiling_ms"]
    min_success = baseline["min_success_rate_pct"]
    problems = []
    if p50 > ceiling:
        problems.append(f"TTI P50 {p50:.1f} ms exceeds the {ceiling} ms ceiling")
    if success < min_success:
        problems.append(f"success rate {success:.1f}% is under the {min_success}% floor")
    if problems:
        return 1, f"REGRESSION: {'; '.join(problems)} (P50={p50:.1f} ms, success={success:.1f}%)"
    return 0, f"OK: TTI P50 {p50:.1f} ms <= {ceiling} ms, success {success:.1f}% >= {min_success}%"


SELF_TEST_CASES = [
    # (summary, baseline, want_code)
    ("  P50      96.8\n  success rate: 100/100 (100.0%), rate-limited retries: 0",
     {"tti_p50_ceiling_ms": 125, "min_success_rate_pct": 95}, 0),
    ("  P50     140.0\n  success rate: 100/100 (100.0%), rate-limited retries: 0",
     {"tti_p50_ceiling_ms": 125, "min_success_rate_pct": 95}, 1),
    ("  P50      96.8\n  success rate: 88/100 (88.0%), rate-limited retries: 0",
     {"tti_p50_ceiling_ms": 125, "min_success_rate_pct": 95}, 1),
    ("no summary here", {"tti_p50_ceiling_ms": 125, "min_success_rate_pct": 95}, 2),
]


def self_test():
    ok = True
    for i, (text, base, want) in enumerate(SELF_TEST_CASES):
        code, msg = verdict(text, base)
        status = "ok" if code == want else "FAIL"
        if code != want:
            ok = False
        print(f"  case {i}: want {want} got {code} [{status}] {msg}")
    print("SELF-TEST PASS" if ok else "SELF-TEST FAIL")
    return 0 if ok else 1


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("summary", nargs="?", help="path to the harness summary (default stdin)")
    ap.add_argument("--baseline", default="bench/canary-baseline.json")
    ap.add_argument("--self-test", action="store_true", help="validate the parser on known cases and exit")
    args = ap.parse_args()
    if args.self_test:
        sys.exit(self_test())
    text = open(args.summary).read() if args.summary else sys.stdin.read()
    with open(args.baseline) as f:
        baseline = json.load(f)
    code, msg = verdict(text, baseline)
    print(msg)
    sys.exit(code)


if __name__ == "__main__":
    main()
