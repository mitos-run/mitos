"""Hosted quickstart: the snippet shown in the repository and Python SDK READMEs.

This file is the checked copy of the hosted "Quickstart" hero so the README code
cannot drift from the real SDK surface. The sdk-examples CI job byte-compiles it
and imports it; the module-level guard below asserts every symbol the hero calls
still exists, so a renamed or removed method fails the build at import time.

It is NOT executed in CI: the hosted default talks to https://mitos.run with a
real MITOS_API_KEY, which CI does not carry. End-to-end execution of the direct
path is proven against a real KVM sandbox-server by examples/direct.py in the
kvm-test job. Run this one yourself with an API key in the environment::

    export MITOS_API_KEY=sk-...
    python3 quickstart.py
"""

import mitos
from mitos.direct import DirectSandbox, DirectSandboxFiles

# Drift guard: the hero below calls these. Asserting them at import time turns the
# CI import-check into a real API-surface check (not just a syntax check), so a
# rename in the SDK fails this example before the README can ship stale.
assert callable(mitos.create)
assert all(hasattr(DirectSandbox, m) for m in ("exec", "run_code", "fork", "terminate"))
assert hasattr(DirectSandboxFiles, "write")


def main() -> None:
    # MITOS_API_KEY from the environment; base URL defaults to https://mitos.run.
    sb = mitos.create("python")
    print(sb.exec("echo hello").stdout)              # hello

    # Files and a stateful code interpreter hang off the same flat handle.
    sb.files.write("/workspace/plan.txt", "draft")

    # N-way copy-on-write fork of the live VM: each sibling lands warm.
    fork_a, fork_b = sb.fork(2)                       # two independent siblings
    fork_a.exec("echo a > /workspace/a.txt")
    fork_b.exec("echo b > /workspace/b.txt")          # b does not see a's write

    ex = sb.run_code("import math; math.sqrt(144)")
    print(ex.text)                                    # 12.0

    sb.terminate()


if __name__ == "__main__":
    main()
