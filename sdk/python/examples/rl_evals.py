"""RL / evals: a SWE-bench-style harness that forks many environments from one golden state.

Use case hook: set up one golden environment (repo checked out, deps installed,
fixtures in place), snapshot it, then fork it once per task instance so every
candidate runs against a byte-identical starting state. Run the grader command in
each fork, collect pass/fail, and report the aggregate pass-rate. Because each
fork starts from the same golden snapshot, the only variable is the candidate, so
the score is reproducible.

This rides the flat mitos.create() / DirectSandbox path:

    golden = mitos.create("python")    # set up the golden state once
    envs = golden.fork(len(instances)) # one isolated env per instance
    # apply each candidate, run the grader, collect exit codes

Hosted vs standalone (honest):
  - Default (no MITOS_BASE_URL): hosted control plane at https://mitos.run with
    MITOS_API_KEY from the environment.
  - Standalone: run a sandbox-server and set MITOS_BASE_URL (or pass argv[1]).

The instances below are a self-contained toy grader (a tiny arithmetic task with
a known fix), so the harness shape is real even without a SWE-bench dataset. Swap
the setup, candidate, and grader strings for a real benchmark.

Run::

    export MITOS_API_KEY=sk-...        # hosted
    python3 rl_evals.py
    # or, standalone:
    python3 rl_evals.py http://localhost:8080

Byte-compiled and import-checked by the sdk-examples CI job; not executed there
(no VM). The asserts below run at import time as a real API-surface check.
"""

import os
import sys
from concurrent.futures import ThreadPoolExecutor
from dataclasses import dataclass
from typing import Optional

import mitos
from mitos.direct import DirectSandbox

# Drift guard: assert the surface the example uses, so a rename fails import.
assert callable(mitos.create)
assert all(hasattr(DirectSandbox, m) for m in ("fork", "exec", "terminate"))


@dataclass
class Instance:
    """One task instance: a candidate solution and the grader that scores it."""

    name: str
    # The candidate writes a module; the grader imports it and asserts. A passing
    # candidate makes the grader exit 0, a failing one exits non-zero.
    candidate: str
    grader: str


# A toy "benchmark": two instances, one whose candidate is correct and one whose
# candidate is buggy, so the harness reports a 1/2 pass-rate deterministically.
INSTANCES = [
    Instance(
        name="add-correct",
        candidate="def solve(a, b):\n    return a + b\n",
        grader="from solution import solve; assert solve(2, 3) == 5",
    ),
    Instance(
        name="add-buggy",
        candidate="def solve(a, b):\n    return a - b\n",
        grader="from solution import solve; assert solve(2, 3) == 5",
    ),
]


@dataclass
class EvalResult:
    instance: str
    passed: bool
    detail: str


def grade(env: DirectSandbox, inst: Instance) -> EvalResult:
    """Apply one candidate to a forked env and run its grader."""
    env.files.write("/workspace/solution.py", inst.candidate)
    result = env.exec(
        f"cd /workspace && python3 -c {inst.grader!r}",
    )
    passed = result.exit_code == 0
    detail = "ok" if passed else (result.stderr.strip().splitlines() or ["assert failed"])[-1]
    return EvalResult(instance=inst.name, passed=passed, detail=detail)


def main(base_url: Optional[str]) -> None:
    # Golden state: in a real harness you would check out the repo and install
    # deps here, so every fork inherits the identical prepared environment.
    golden = mitos.create("python", base_url=base_url)
    envs: list[DirectSandbox] = []
    try:
        envs = golden.fork(len(INSTANCES))
        print(f"forked {len(envs)} eval environments from golden {golden.id}")

        with ThreadPoolExecutor(max_workers=len(envs)) as pool:
            results = list(
                pool.map(lambda pair: grade(*pair), zip(envs, INSTANCES))
            )

        passed = 0
        for r in results:
            mark = "PASS" if r.passed else "FAIL"
            print(f"  [{mark}] {r.instance}: {r.detail}")
            passed += int(r.passed)

        total = len(results)
        print(f"pass-rate: {passed}/{total}")
        # The toy benchmark is constructed to score exactly 1/2; assert it so the
        # harness itself is self-checking when run end to end.
        if passed != 1:
            raise SystemExit(f"example FAILED: expected 1 pass, got {passed}")
        print("rl_evals example OK")
    finally:
        for env in envs:
            try:
                env.terminate()
            except Exception:
                pass
        golden.terminate()


if __name__ == "__main__":
    url = sys.argv[1] if len(sys.argv) > 1 else os.environ.get("MITOS_BASE_URL")
    main(url)
