"""Third-party agent-framework adapters for the mitos sandbox.

Each adapter maps a framework's sandbox-backend interface onto the native mitos
SDK surface (``mitos.create`` / ``DirectSandbox``: exec, files, run_code, fork).
The adapters are thin: the engine work lives in the SDK and sandbox-server, so
an adapter is wiring, not new behavior.

The framework dependency is always OPTIONAL. Importing an adapter module pulls
in mitos only; the framework's own types are referenced lazily (inside
TYPE_CHECKING or function bodies), so the SDK test suite runs without the
framework installed.

Shared wire-op mapping lives in ``mitos.integrations._mapping`` so the
LangChain adapter (#203), the OpenAI / Claude adapters (#204), and the E2B
compat shim (#206) reuse one translation layer instead of three.
"""

from mitos.integrations._mapping import (
    OpsTarget,
    execution_to_dict,
    map_execute,
    map_files_list,
    map_files_read,
    map_files_write,
    map_run_code,
)

__all__ = [
    "OpsTarget",
    "execution_to_dict",
    "map_execute",
    "map_files_list",
    "map_files_read",
    "map_files_write",
    "map_run_code",
]
