"""Best-of-N: fork one warm sandbox into N, run attempts in parallel, keep the winner.

Use case hook: an agent has one warm, set-up environment and wants N independent
attempts at a task (different prompts, seeds, or candidate patches). Fork the live
sandbox into N copy-on-write siblings, run each attempt concurrently, score the
results, keep the best, and discard the rest. Each sibling is fully isolated: a
write in one is invisible to the others.

This rides the flat mitos.create() / DirectSandbox path:

    source = mitos.create("python")
    children = source.fork(N)          # N independent warm siblings
    # run an attempt in each child concurrently, score, keep the winner

fork(N) here is a loop of single forks under the hood (the standalone server
re-forks the template per child); the ergonomic stays one call. The siblings run
concurrently from the client via a thread pool, since each exec is an independent
HTTP stream.

Hosted vs standalone (honest):
  - Default (no MITOS_BASE_URL): hosted control plane at https://mitos.run with
    MITOS_API_KEY from the environment.
  - Standalone: run a sandbox-server and set MITOS_BASE_URL (or pass argv[1]).

Run::

    export MITOS_API_KEY=sk-...        # hosted
    python3 best_of_n.py
    # or, standalone:
    python3 best_of_n.py http://localhost:8080

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

N = 4

# A toy task: each attempt computes a candidate and prints a score on the last
# line. A real harness would run a model, a candidate patch, or a search seed
# here; the shape (run, capture, score) is the same.
ATTEMPT_CODE = (
    "python3 -c \""
    "seed={seed}; "
    "score=(seed*7 + 13) % 17; "
    "print(f'attempt seed={{seed}} score={{score}}')\""
)


@dataclass
class Attempt:
    child: DirectSandbox
    seed: int
    score: int
    output: str


def run_attempt(child: DirectSandbox, seed: int) -> Attempt:
    """Run one attempt in its own sandbox and parse the score off the last line."""
    result = child.exec(ATTEMPT_CODE.format(seed=seed))
    if result.exit_code != 0:
        raise SystemExit(
            f"attempt seed={seed} FAILED: exit {result.exit_code} stderr={result.stderr!r}"
        )
    last = result.stdout.strip().splitlines()[-1]
    score = int(last.rsplit("score=", 1)[1])
    return Attempt(child=child, seed=seed, score=score, output=last)


def main(base_url: Optional[str]) -> None:
    source = mitos.create("python", base_url=base_url)
    children: list[DirectSandbox] = []
    try:
        # One warm parent, N copy-on-write siblings.
        children = source.fork(N)
        print(f"forked {len(children)} siblings from {source.id}")

        # Run all attempts concurrently; each exec is an independent stream.
        with ThreadPoolExecutor(max_workers=N) as pool:
            attempts = list(
                pool.map(lambda pair: run_attempt(*pair),
                         [(child, seed) for seed, child in enumerate(children)])
            )

        for a in attempts:
            print(f"  {a.output}")

        winner = max(attempts, key=lambda a: a.score)
        print(f"winner: seed={winner.seed} score={winner.score}")

        # Keep the winner (here we just report it); discard the losing siblings.
        for a in attempts:
            if a is not winner:
                a.child.terminate()
        print("best_of_n example OK")
    finally:
        # Terminate the winner (if any) and the source. Already-terminated
        # losers are skipped; terminate is idempotent enough for cleanup.
        for child in children:
            try:
                child.terminate()
            except Exception:
                pass
        source.terminate()


if __name__ == "__main__":
    url = sys.argv[1] if len(sys.argv) > 1 else os.environ.get("MITOS_BASE_URL")
    main(url)
