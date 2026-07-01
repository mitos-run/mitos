"""Code interpreter: a stateful run_code data-analysis loop that returns a chart.

Use case hook: hand an agent a sandboxed Python kernel, feed it cells, and get
back stdout plus rich display artifacts (the last-expression value, a PNG chart,
structured chart JSON). State persists across run_code calls for the sandbox
lifetime, so the loop builds up a dataframe in one cell and plots it in the next.

This rides the flat mitos.create() / DirectSandbox path:

    sb = mitos.create("python")
    ex = sb.run_code("import pandas as pd; ...")
    ex.results[0].png        # base64 PNG of a matplotlib figure, when produced

Hosted vs standalone (honest):
  - Default (no MITOS_BASE_URL): talks to the hosted control plane at
    https://mitos.run with MITOS_API_KEY from the environment.
  - Standalone: run a sandbox-server (cmd/sandbox-server) and set
    MITOS_BASE_URL=http://localhost:8080 (or pass it as argv[1]).
  - Either way run_code needs a base image carrying the code-interpreter kernel
    (pandas + matplotlib for the plotting cell below); without it the Execution
    comes back with a KernelUnavailable error, which this example reports.

Run::

    export MITOS_API_KEY=sk-...        # hosted
    python3 code_interpreter.py
    # or, standalone:
    python3 code_interpreter.py http://localhost:8080

This example is byte-compiled and import-checked by the sdk-examples CI job; it
is NOT executed there (no kernel-backed VM in that job). The asserts below run at
import time and turn the import-check into a real API-surface check.
"""

import os
import sys

import mitos
from mitos.direct import DirectSandbox
from mitos.types import Execution, Result

# Drift guard: the loop below calls these. Asserting them at import time makes
# the CI import-check fail if the SDK renames or drops a symbol the example uses.
assert callable(mitos.create)
assert all(hasattr(DirectSandbox, m) for m in ("run_code", "exec", "terminate"))
assert all(hasattr(Result, p) for p in ("png", "text", "chart"))
assert hasattr(Execution, "results") or "results" in Execution.__dataclass_fields__


# Each cell is one run_code call; the kernel keeps state between them, so the
# frame built in cell 1 is still in scope when cell 3 plots it.
CELLS = [
    # 1. Build a small dataset in the kernel.
    """
import pandas as pd
df = pd.DataFrame({"day": [1, 2, 3, 4, 5], "signups": [10, 14, 9, 22, 31]})
df.shape
""",
    # 2. Reduce over the state from the previous cell.
    "total = int(df['signups'].sum()); total",
    # 3. Plot the state and let the kernel emit the figure as a rich result.
    """
import matplotlib
matplotlib.use("Agg")
import matplotlib.pyplot as plt
fig, ax = plt.subplots()
ax.plot(df["day"], df["signups"], marker="o")
ax.set_title("signups by day")
fig  # last expression: the kernel renders it as an image/png result
""",
]


def run_cell(sandbox: DirectSandbox, code: str) -> Execution:
    """Run one cell and print its streamed logs and last-expression value."""
    ex = sandbox.run_code(
        code,
        on_stdout=lambda line: print(f"  stdout: {line}", end=""),
        on_stderr=lambda line: print(f"  stderr: {line}", end=""),
    )
    if ex.error is not None:
        # KernelUnavailable shows up here on a base image without the kernel.
        raise SystemExit(
            f"run_code error: {ex.error.name}: {ex.error.value}\n"
            "Use a base image that carries the code-interpreter kernel "
            "(pandas + matplotlib for the plotting cell)."
        )
    if ex.text is not None:
        print(f"  => {ex.text}")
    return ex


def main(base_url: str | None) -> None:
    # base_url=None lets the SDK resolve MITOS_BASE_URL, else the hosted default.
    sandbox = mitos.create("python", base_url=base_url)
    try:
        chart_png: str | None = None
        for i, code in enumerate(CELLS, start=1):
            print(f"cell {i}:")
            ex = run_cell(sandbox, code)
            for result in ex.results:
                if result.png is not None:
                    chart_png = result.png

        if chart_png is None:
            raise SystemExit(
                "example FAILED: no chart was produced (expected an image/png "
                "result from the plotting cell)"
            )
        # The PNG is base64; in a real loop you would decode and save or display
        # it. Here we just prove one came back.
        print(f"chart produced: {len(chart_png)} base64 chars of image/png")
        print("code_interpreter example OK")
    finally:
        sandbox.terminate()


if __name__ == "__main__":
    url = sys.argv[1] if len(sys.argv) > 1 else os.environ.get("MITOS_BASE_URL")
    main(url)
