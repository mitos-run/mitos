"""Direct-mode example: the standalone sandbox-server, no Kubernetes required.

Run a sandbox-server (cmd/sandbox-server) and then::

    python3 direct.py [base_url]

It creates a template, forks a sandbox, runs a command in it, prints the result,
and terminates. The base URL comes from argv[1], else MITOS_BASE_URL, else
http://localhost:8080. This example is executed end to end against a real KVM
sandbox-server by the sdk-examples-kvm CI phase, so it is kept runnable, not just
illustrative.
"""

import os
import sys

from mitos.direct import SandboxServer


def main() -> None:
    base_url = (
        sys.argv[1]
        if len(sys.argv) > 1
        else os.environ.get("MITOS_BASE_URL", "http://localhost:8080")
    )
    server = SandboxServer(base_url)

    server.create_template("python-312")
    sandbox = server.fork("python-312", "example-sandbox")
    try:
        result = sandbox.exec("echo hello from the sandbox")
        print(f"exec exit={result.exit_code} stdout={result.stdout!r}")
        if result.exit_code != 0:
            raise SystemExit(f"example FAILED: exec returned exit {result.exit_code}")
        if "hello from the sandbox" not in result.stdout:
            raise SystemExit(f"example FAILED: unexpected stdout {result.stdout!r}")
    finally:
        sandbox.terminate()
        print("sandbox terminated")
    print("direct example OK")


if __name__ == "__main__":
    main()
