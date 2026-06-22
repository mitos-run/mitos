"""SDK conformance parity suite (issue #22): Python runner.

This runs the SHARED scenario in sdk/conformance/scenario.json against a live
standalone sandbox-server in mock mode and asserts that each step's NORMALIZED
result equals the shared expectation. The TypeScript runner
(sdk/typescript/test/conformance.test.ts) runs the IDENTICAL scenario against
the SAME server and asserts the IDENTICAL normalized expectations, so the two
languages prove byte-equal logical behavior for the shared control plane.

Scope: the mock engine has NO guest VM, so exec / files / run_code are out of
scope here (they need a vsock guest agent, proven on the KVM CI job). The
conformance SURFACE is the control plane both SDKs share: create template, list
templates, fork, list sandboxes, terminate.

Gating (matches the existing integration tests test_direct.py / test_e2b_compat.py
which target a live server and skip when it is absent): this test SKIPS unless a
reachable server URL is provided, so the unit-only ``make test-python`` is
unaffected. The URL comes from MITOS_CONFORMANCE_URL, else the default
http://localhost:8080; if nothing answers /v1/health, the test skips.
"""

import json
import os
from pathlib import Path

import httpx
import pytest

from mitos.direct import SandboxServer


# Resolve the shared scenario, defined ONCE for both languages.
SCENARIO_PATH = (
    Path(__file__).resolve().parents[3] / "sdk" / "conformance" / "scenario.json"
)
SCENARIO = json.loads(SCENARIO_PATH.read_text())

# Default to the standard standalone port; MITOS_CONFORMANCE_URL overrides it.
DEFAULT_URL = "http://localhost:8080"


def _resolve_url() -> str | None:
    """The conformance server URL, or None to skip.

    Opt-in: an explicit MITOS_CONFORMANCE_URL is used as-is. Otherwise the
    default localhost:8080 is probed and only used if it answers /v1/health,
    so a developer's unit-only run (no server) skips cleanly.
    """
    url = os.environ.get("MITOS_CONFORMANCE_URL")
    if url:
        return url.rstrip("/")
    probe = DEFAULT_URL
    try:
        r = httpx.get(f"{probe}/v1/health", timeout=0.5)
        if r.status_code == 200:
            return probe
    except (httpx.HTTPError, OSError):
        return None
    return None


SERVER_URL = _resolve_url()

pytestmark = pytest.mark.skipif(
    SERVER_URL is None,
    reason=(
        "no conformance server: set MITOS_CONFORMANCE_URL or run "
        "`go run ./cmd/sandbox-server --mock` on localhost:8080"
    ),
)

# Stable keys to compare, per the scenario's normalization contract.
_TEMPLATE_KEYS = SCENARIO["normalization"]["template_keys"]
_SANDBOX_LIST_KEYS = SCENARIO["normalization"]["sandbox_list_keys"]


def _norm_template(t: dict) -> dict:
    """Normalize a template (SDK dict or wire dict) to the stable keys.

    The Python SDK returns the raw wire dict (snake_case), so only key
    selection is needed; timing fields and the network echo are dropped.
    """
    return {k: t[k] for k in _TEMPLATE_KEYS}


def _norm_sandbox_list_entry(s: dict) -> dict:
    """Normalize a list_sandboxes entry to the stable keys (id, template_id)."""
    return {k: s[k] for k in _SANDBOX_LIST_KEYS}


def _step(name: str) -> dict:
    for s in SCENARIO["steps"]:
        if s["name"] == name:
            return s
    raise KeyError(name)


def test_conformance_scenario():
    """Run every shared scenario step in order and assert the normalized result
    equals the shared expectation. This is the SAME scenario the TypeScript
    runner executes against the SAME server."""
    server = SandboxServer(SERVER_URL)
    template_id = SCENARIO["ids"]["template"]
    sandbox_id = SCENARIO["ids"]["sandbox"]

    # Clean slate: a prior run may have left the sandbox behind (templates are
    # get-or-create, so a lingering template is harmless and re-created).
    for s in server.list_sandboxes():
        if s["id"] == sandbox_id:
            httpx.delete(f"{SERVER_URL}/v1/sandboxes/{sandbox_id}", timeout=5)

    # Step 1: createTemplate(id) -> {id, ready}.
    step = _step("create_template")
    created = server.create_template(
        step["args"]["id"], init_wait_seconds=step["args"]["init_wait_seconds"]
    )
    assert _norm_template(created) == step["expect"], "create_template"

    # Step 2: listTemplates() contains the template.
    step = _step("list_templates_contains")
    templates = [_norm_template(t) for t in server.list_templates()]
    assert step["expect_contains"] in templates, "list_templates_contains"

    # Step 3: fork(template, id) -> {id, endpoint present}.
    step = _step("fork")
    sandbox = server.fork(step["args"]["template"], step["args"]["id"])
    fork_norm = {
        "id": sandbox.id,
        "endpoint_present": bool(sandbox.endpoint),
    }
    assert fork_norm == step["expect"], "fork"

    # Step 4: listSandboxes() contains the sandbox with {id, template_id}.
    step = _step("list_sandboxes_contains")
    sandboxes = [_norm_sandbox_list_entry(s) for s in server.list_sandboxes()]
    assert step["expect_contains"] in sandboxes, "list_sandboxes_contains"

    # Step 5: terminate() -> the sandbox is gone from listSandboxes().
    step = _step("terminate")
    sandbox.terminate()
    remaining_ids = [s["id"] for s in server.list_sandboxes()]
    assert step["expect_absent_from_sandboxes"] not in remaining_ids, "terminate"
