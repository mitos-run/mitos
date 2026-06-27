#!/usr/bin/env python3
"""Reproducible end to end verification of the HOSTED Mitos service.

This harness proves a brand new user can use the hosted service exactly as the
docs describe: sign up, receive the free signup credit, and run the quickstart to
create and FORK a sandbox, across a set of user variations (different SDKs,
adapters, and templates).

It is split into two halves so the cheap half can run before the cluster exists:

  Steps 1 to 4 (the signup and credit path) hit the console and onboarding HTTP
  API only. They run with the QA token seam (the gated GET /onboarding/e2e/token
  endpoint) and need NO KVM. Run them with --only-signup.

  Step 5 (the quickstart) drives a chosen surface (a published SDK, the CLI, the
  MCP server, or a framework adapter) against the SDK gateway. It creates a
  sandbox, execs, FORKS, execs in each fork, asserts outputs, and terminates.
  This half needs the live KVM cluster behind the gateway.

Every user prints PASS, FAIL, or SKIPPED with the failing step and the error. The
process exits non-zero if ANY selected user fails. A surface whose tool is not
installed is honestly marked SKIPPED with the reason, never silently passed.

Standard library only, plus the published SDKs for the in process surfaces. No
third party HTTP client. See README.md for the exact run commands.
"""

from __future__ import annotations

import argparse
import json
import os
import shutil
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.parse
import urllib.request
from dataclasses import dataclass, field
from typing import Any, Callable, Optional

# ----------------------------------------------------------------------------
# Configuration (env with sane defaults)
# ----------------------------------------------------------------------------


@dataclass
class Config:
    """Runtime configuration resolved from the environment.

    base_url is the console and onboarding origin (signup, verify, billing).
    api_url is the SDK gateway origin (create, exec, fork, terminate). The docs
    default the SDK base URL to https://mitos.run; the hosted gateway is split out
    here as api.mitos.run so the harness can target each half independently.
    """

    base_url: str
    api_url: str
    e2e_token: str
    e2e_domain: str
    expected_credit_cents: int
    cli_kubeconfig: str
    http_timeout: float
    repo_root: str

    @classmethod
    def from_env(cls) -> "Config":
        return cls(
            base_url=os.environ.get("MITOS_BASE_URL", "https://mitos.run").rstrip("/"),
            api_url=os.environ.get("MITOS_API_URL", "https://api.mitos.run").rstrip("/"),
            e2e_token=os.environ.get("MITOS_E2E_TOKEN", ""),
            e2e_domain=os.environ.get("MITOS_E2E_DOMAIN", "e2e.mitos.run").strip().lower(),
            expected_credit_cents=int(os.environ.get("MITOS_E2E_EXPECTED_CREDIT_CENTS", "500")),
            cli_kubeconfig=os.environ.get("MITOS_E2E_CLI_KUBECONFIG", ""),
            http_timeout=float(os.environ.get("MITOS_E2E_HTTP_TIMEOUT", "30")),
            repo_root=_find_repo_root(),
        )


def _find_repo_root() -> str:
    """Walk up from this file to the repo root (the dir holding go.mod)."""
    here = os.path.dirname(os.path.abspath(__file__))
    cur = here
    for _ in range(8):
        if os.path.exists(os.path.join(cur, "go.mod")) and os.path.isdir(
            os.path.join(cur, "sdk")
        ):
            return cur
        parent = os.path.dirname(cur)
        if parent == cur:
            break
        cur = parent
    # Fall back to two levels up from test/hosted-e2e/.
    return os.path.dirname(os.path.dirname(here))


# ----------------------------------------------------------------------------
# Result model
# ----------------------------------------------------------------------------

PASS = "PASS"
FAIL = "FAIL"
SKIPPED = "SKIPPED"


class SkipSurface(Exception):
    """Raised by a surface when its tool or SDK is unavailable.

    The user is marked SKIPPED with this reason, not PASS and not FAIL, so a
    missing dependency never masquerades as a green run.
    """


class StepError(Exception):
    """A failure tied to a named step, so the summary can name what broke."""

    def __init__(self, step: str, detail: str):
        super().__init__(f"{step}: {detail}")
        self.step = step
        self.detail = detail


@dataclass
class UserResult:
    n: int
    surface: str
    email: str
    status: str = PASS
    failed_step: str = ""
    detail: str = ""
    api_key_tail: str = ""
    credit_cents: Optional[int] = None
    steps_done: list[str] = field(default_factory=list)
    sandbox_ids: list[str] = field(default_factory=list)


# ----------------------------------------------------------------------------
# HTTP helpers (stdlib urllib)
# ----------------------------------------------------------------------------


@dataclass
class HTTPResponse:
    status: int
    body_text: str
    headers: Any  # http.client.HTTPMessage
    set_cookies: list[str]

    def json(self) -> Any:
        return json.loads(self.body_text) if self.body_text else None


def http_request(
    method: str,
    url: str,
    *,
    headers: Optional[dict[str, str]] = None,
    json_body: Optional[dict[str, Any]] = None,
    cookie: Optional[str] = None,
    timeout: float = 30.0,
) -> HTTPResponse:
    """Issue one HTTP request and return status, body, headers, and Set-Cookie.

    A non-2xx status is returned, NOT raised, so each step decides what an
    unexpected status means. Network errors raise StepError-free exceptions that
    the caller wraps with the step name.
    """
    hdrs = dict(headers or {})
    data = None
    if json_body is not None:
        data = json.dumps(json_body).encode("utf-8")
        hdrs.setdefault("Content-Type", "application/json")
    if cookie:
        hdrs["Cookie"] = cookie

    req = urllib.request.Request(url, data=data, headers=hdrs, method=method)
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            body = resp.read().decode("utf-8", "replace")
            return HTTPResponse(
                status=resp.status,
                body_text=body,
                headers=resp.headers,
                set_cookies=resp.headers.get_all("Set-Cookie") or [],
            )
    except urllib.error.HTTPError as e:
        body = e.read().decode("utf-8", "replace") if e.fp else ""
        return HTTPResponse(
            status=e.code,
            body_text=body,
            headers=e.headers,
            set_cookies=(e.headers.get_all("Set-Cookie") or []) if e.headers else [],
        )


def _extract_cookie(set_cookies: list[str], name: str) -> Optional[str]:
    """Pull the name=value pair for a cookie out of Set-Cookie header values."""
    for raw in set_cookies:
        first = raw.split(";", 1)[0].strip()
        if first.startswith(name + "="):
            return first
    return None


# ----------------------------------------------------------------------------
# Steps 1 to 4: signup, token, verify, credit
# ----------------------------------------------------------------------------


@dataclass
class SignupOutcome:
    api_key: str
    session_cookie: str
    account_id: str
    org_id: str
    credit_cents: int


def step_signup(cfg: Config, email: str) -> None:
    """Step 1: POST /onboarding/signup -> 202 accepted (uniform body)."""
    r = http_request(
        "POST",
        f"{cfg.base_url}/onboarding/signup",
        json_body={"email": email},
        timeout=cfg.http_timeout,
    )
    if r.status != 202:
        raise StepError("signup", f"expected 202, got {r.status}: {r.body_text[:200]}")


def step_get_e2e_token(cfg: Config, email: str) -> str:
    """Step 2: GET the gated QA token seam -> 200 {"token": <raw>}.

    This is the PR #515 endpoint GET /onboarding/e2e/token, mounted only when the
    console runs with MITOS_CONSOLE_E2E plus a bearer and a domain allowlist. The
    bearer rides in Authorization; the raw verification token comes back in the
    body so the harness can drive verify without reading a mailbox.
    """
    if not cfg.e2e_token:
        raise StepError(
            "e2e_token",
            "MITOS_E2E_TOKEN is empty; the QA token seam needs the shared bearer",
        )
    url = f"{cfg.base_url}/onboarding/e2e/token?email={urllib.parse.quote(email)}"
    r = http_request(
        "GET",
        url,
        headers={"Authorization": f"Bearer {cfg.e2e_token}"},
        timeout=cfg.http_timeout,
    )
    if r.status != 200:
        raise StepError(
            "e2e_token",
            f"expected 200, got {r.status} (gate failed: bearer, domain, or sink): {r.body_text[:200]}",
        )
    token = (r.json() or {}).get("token", "")
    if not token:
        raise StepError("e2e_token", "200 but no token field in body")
    return token


def step_verify(cfg: Config, token: str) -> tuple[str, str, str, str]:
    """Step 3: POST /onboarding/verify -> 200 with apiKey and a session cookie.

    Returns (api_key, session_cookie, account_id, org_id). The raw first key is
    shown exactly once here on a fresh verify; the mitos_session cookie logs the
    new user straight into the console.
    """
    r = http_request(
        "POST",
        f"{cfg.base_url}/onboarding/verify",
        json_body={"token": token},
        timeout=cfg.http_timeout,
    )
    if r.status != 200:
        raise StepError("verify", f"expected 200, got {r.status}: {r.body_text[:200]}")
    body = r.json() or {}
    api_key = body.get("apiKey", "")
    if not api_key:
        raise StepError(
            "verify",
            "200 but no apiKey (an idempotent re-verify returns no key; expected a fresh signup)",
        )
    if not api_key.startswith("sk-"):
        raise StepError("verify", f"apiKey does not look like an sk- key: {api_key[:6]}...")
    cookie = _extract_cookie(r.set_cookies, "mitos_session")
    if not cookie:
        raise StepError("verify", "200 with apiKey but no mitos_session Set-Cookie")
    return api_key, cookie, body.get("accountId", ""), body.get("orgId", "")


def step_credit_check(cfg: Config, session_cookie: str) -> int:
    """Step 4: GET /console/billing with the session cookie -> 200 balance check.

    Asserts balance_cents equals the expected signup credit (default 500, that is
    the $5 grant). The expected value is configurable via
    MITOS_E2E_EXPECTED_CREDIT_CENTS to track the deployment's configured grant.
    """
    r = http_request(
        "GET",
        f"{cfg.base_url}/console/billing",
        cookie=session_cookie,
        timeout=cfg.http_timeout,
    )
    if r.status != 200:
        raise StepError("credit_check", f"expected 200, got {r.status}: {r.body_text[:200]}")
    body = r.json() or {}
    if "balance_cents" not in body:
        raise StepError("credit_check", f"200 but no balance_cents field: {r.body_text[:200]}")
    balance = int(body["balance_cents"])
    if balance != cfg.expected_credit_cents:
        raise StepError(
            "credit_check",
            f"balance_cents is {balance}, expected {cfg.expected_credit_cents} "
            f"(the ${cfg.expected_credit_cents / 100:.2f} signup credit)",
        )
    return balance


def run_signup_path(cfg: Config, email: str, res: UserResult) -> SignupOutcome:
    """Run steps 1 to 4 and record progress on res. Raises StepError on failure."""
    step_signup(cfg, email)
    res.steps_done.append("signup(202)")

    token = step_get_e2e_token(cfg, email)
    res.steps_done.append("e2e_token(200)")

    api_key, cookie, account_id, org_id = step_verify(cfg, token)
    res.api_key_tail = api_key[-4:]
    res.steps_done.append("verify(apiKey+cookie)")

    credit = step_credit_check(cfg, cookie)
    res.credit_cents = credit
    res.steps_done.append(f"credit({credit}c)")

    return SignupOutcome(
        api_key=api_key,
        session_cookie=cookie,
        account_id=account_id,
        org_id=org_id,
        credit_cents=credit,
    )


# ----------------------------------------------------------------------------
# Step 5 surfaces: the quickstart per surface
# ----------------------------------------------------------------------------
#
# Each surface function signature is (cfg, api_key, res) and it must:
#   create a sandbox, exec or run_code, FORK into N, exec in each fork, assert,
#   terminate every sandbox it made.
# It raises SkipSurface(reason) when its tool or SDK is unavailable, and
# StepError(step, detail) on an assertion or call failure. Best effort cleanup
# runs even on failure.

FORK_COUNT = 2


def _require_python_sdk() -> Any:
    try:
        import mitos  # noqa: F401

        return mitos
    except Exception as e:
        raise SkipSurface(f"mitos-run not importable ({e}); pip install mitos-run") from e


def surface_python_sync(cfg: Config, api_key: str, res: UserResult) -> None:
    """Surface 1: Python SDK sync. The headline docs/quickstart.md snippet."""
    mitos = _require_python_sdk()
    sb = None
    forks: list[Any] = []
    try:
        sb = mitos.create("python", api_key=api_key, base_url=cfg.api_url)
        res.sandbox_ids.append(getattr(sb, "id", "?"))

        ex = sb.run_code("print(1 + 1)")
        if (ex.text or "").strip() not in ("2", "2\n"):
            # run_code last-value or stdout: assert via a deterministic expression.
            ex2 = sb.run_code("21 * 2")
            if (ex2.text or "").strip() != "42":
                raise StepError("run_code", f"expected 42, got text={ex2.text!r}")

        r = sb.exec("echo hello-sync")
        if "hello-sync" not in r.stdout:
            raise StepError("exec", f"expected hello-sync in stdout, got {r.stdout!r}")

        forks = sb.fork(FORK_COUNT)
        if len(forks) != FORK_COUNT:
            raise StepError("fork", f"expected {FORK_COUNT} forks, got {len(forks)}")
        for i, f in enumerate(forks):
            res.sandbox_ids.append(getattr(f, "id", "?"))
            marker = f"fork-{i}"
            fr = f.exec(f"echo {marker}")
            if marker not in fr.stdout:
                raise StepError("fork_exec", f"fork {i}: expected {marker}, got {fr.stdout!r}")
    finally:
        for f in forks:
            _safe(lambda f=f: f.terminate())
        if sb is not None:
            _safe(sb.terminate)


def surface_python_async(cfg: Config, api_key: str, res: UserResult) -> None:
    """Surface 2: Python SDK async (mitos.aio)."""
    _require_python_sdk()
    try:
        import asyncio

        import mitos.aio as aio
    except Exception as e:
        raise SkipSurface(f"mitos.aio not importable ({e})") from e

    async def main() -> None:
        sb = await aio.create("python", api_key=api_key, base_url=cfg.api_url)
        res.sandbox_ids.append(getattr(sb, "id", "?"))
        forks: list[Any] = []
        try:
            r = await sb.exec("echo hello-async")
            if "hello-async" not in r.stdout:
                raise StepError("exec", f"expected hello-async, got {r.stdout!r}")
            ex = await sb.run_code("6 * 7")
            if (ex.text or "").strip() != "42":
                raise StepError("run_code", f"expected 42, got {ex.text!r}")
            forks = await sb.fork(FORK_COUNT)
            if len(forks) != FORK_COUNT:
                raise StepError("fork", f"expected {FORK_COUNT}, got {len(forks)}")
            for i, f in enumerate(forks):
                res.sandbox_ids.append(getattr(f, "id", "?"))
                fr = await f.exec(f"echo afork-{i}")
                if f"afork-{i}" not in fr.stdout:
                    raise StepError("fork_exec", f"fork {i}: got {fr.stdout!r}")
        finally:
            for f in forks:
                await _asafe(f.terminate)
            await _asafe(sb.terminate)

    asyncio.run(main())


def surface_python_node_template(cfg: Config, api_key: str, res: UserResult) -> None:
    """Surface 9: Python SDK against a different template (node)."""
    mitos = _require_python_sdk()
    sb = None
    forks: list[Any] = []
    try:
        sb = mitos.create("node", api_key=api_key, base_url=cfg.api_url)
        res.sandbox_ids.append(getattr(sb, "id", "?"))
        r = sb.exec("node -e 'console.log(40 + 2)'")
        if "42" not in r.stdout:
            raise StepError("exec", f"expected 42 from node, got {r.stdout!r}")
        forks = sb.fork(FORK_COUNT)
        if len(forks) != FORK_COUNT:
            raise StepError("fork", f"expected {FORK_COUNT}, got {len(forks)}")
        for i, f in enumerate(forks):
            res.sandbox_ids.append(getattr(f, "id", "?"))
            fr = f.exec(f"node -e \"console.log('nf-{i}')\"")
            if f"nf-{i}" not in fr.stdout:
                raise StepError("fork_exec", f"fork {i}: got {fr.stdout!r}")
    finally:
        for f in forks:
            _safe(lambda f=f: f.terminate())
        if sb is not None:
            _safe(sb.terminate)


def surface_python_runcode_files(cfg: Config, api_key: str, res: UserResult) -> None:
    """Surface 10: files.write + files.read + run_code, then fork.

    Exercises the flat files handle and the stateful interpreter on the same
    sandbox, then forks so each sibling can read the parent's pre-fork write.
    """
    mitos = _require_python_sdk()
    sb = None
    forks: list[Any] = []
    try:
        sb = mitos.create("python", api_key=api_key, base_url=cfg.api_url)
        res.sandbox_ids.append(getattr(sb, "id", "?"))
        sb.files.write("/workspace/plan.txt", "draft-42")
        got = sb.files.read("/workspace/plan.txt")
        if "draft-42" not in got:
            raise StepError("files", f"read back {got!r}, expected draft-42")
        ex = sb.run_code("import math; math.sqrt(144)")
        if (ex.text or "").strip() != "12.0":
            raise StepError("run_code", f"expected 12.0, got {ex.text!r}")
        forks = sb.fork(FORK_COUNT)
        if len(forks) != FORK_COUNT:
            raise StepError("fork", f"expected {FORK_COUNT}, got {len(forks)}")
        for i, f in enumerate(forks):
            res.sandbox_ids.append(getattr(f, "id", "?"))
            # The pre-fork write is part of the forked state.
            fgot = f.files.read("/workspace/plan.txt")
            if "draft-42" not in fgot:
                raise StepError("fork_files", f"fork {i}: read {fgot!r}, expected draft-42")
    finally:
        for f in forks:
            _safe(lambda f=f: f.terminate())
        if sb is not None:
            _safe(sb.terminate)


def surface_openai_agents(cfg: Config, api_key: str, res: UserResult) -> None:
    """Surface 7: OpenAI Agents adapter (mitos.integrations.openai_agents).

    The adapter binds run_command / read_file / write_file / run_code to a Mitos
    sandbox. It is usable without the openai-agents package installed (only
    as_function_tools needs it), so this surface needs only mitos-run. Fork stays
    reachable as the native op via tools.sandbox.fork(n).
    """
    _require_python_sdk()
    try:
        from mitos.integrations.openai_agents import MitosSandboxTools
    except Exception as e:
        raise SkipSurface(f"mitos.integrations.openai_agents not importable ({e})") from e

    tools = None
    forks: list[Any] = []
    try:
        tools = MitosSandboxTools.create("python", api_key=api_key, base_url=cfg.api_url)
        res.sandbox_ids.append(getattr(tools.sandbox, "id", "?"))
        out = tools.run_command("echo oai-hello")
        if "oai-hello" not in out.get("stdout", ""):
            raise StepError("run_command", f"got {out!r}")
        tools.write_file("/workspace/a.txt", "hello")
        if "hello" not in tools.read_file("/workspace/a.txt"):
            raise StepError("files", "read back did not contain hello")
        rc = tools.run_code("import math; math.sqrt(144)")
        if (rc.get("text") or "").strip() != "12.0":
            raise StepError("run_code", f"expected 12.0, got {rc!r}")
        forks = tools.sandbox.fork(FORK_COUNT)
        if len(forks) != FORK_COUNT:
            raise StepError("fork", f"expected {FORK_COUNT}, got {len(forks)}")
        for i, f in enumerate(forks):
            res.sandbox_ids.append(getattr(f, "id", "?"))
            fr = f.exec(f"echo oaif-{i}")
            if f"oaif-{i}" not in fr.stdout:
                raise StepError("fork_exec", f"fork {i}: got {fr.stdout!r}")
    finally:
        for f in forks:
            _safe(lambda f=f: f.terminate())
        if tools is not None:
            _safe(tools.close)


def surface_langchain(cfg: Config, api_key: str, res: UserResult) -> None:
    """Surface 8: LangChain / deepagents adapter (mitos.integrations.langchain).

    MitosSandbox is the Mitos backend for LangChain's pluggable sandbox slot. It
    is usable without langchain installed, and fork is a native op that returns
    sibling MitosSandbox backends.
    """
    _require_python_sdk()
    try:
        from mitos.integrations.langchain import MitosSandbox
    except Exception as e:
        raise SkipSurface(f"mitos.integrations.langchain not importable ({e})") from e

    sb = None
    children: list[Any] = []
    try:
        sb = MitosSandbox.create("python", api_key=api_key, base_url=cfg.api_url)
        out = sb.execute("echo lc-hello")
        if "lc-hello" not in out.get("stdout", ""):
            raise StepError("execute", f"got {out!r}")
        sb.write_file("/workspace/a.txt", "hello")
        if "hello" not in sb.read_file("/workspace/a.txt"):
            raise StepError("files", "read back did not contain hello")
        ex = sb.run_code("import math; math.sqrt(144)")
        if (getattr(ex, "text", None) or "").strip() != "12.0":
            raise StepError("run_code", f"expected 12.0, got {getattr(ex, 'text', None)!r}")
        children = sb.fork(FORK_COUNT)
        if len(children) != FORK_COUNT:
            raise StepError("fork", f"expected {FORK_COUNT}, got {len(children)}")
        for i, c in enumerate(children):
            cout = c.execute(f"echo lcf-{i}")
            if f"lcf-{i}" not in cout.get("stdout", ""):
                raise StepError("fork_exec", f"fork {i}: got {cout!r}")
    finally:
        for c in children:
            _safe(c.close)
        if sb is not None:
            _safe(sb.close)


def surface_typescript(cfg: Config, api_key: str, res: UserResult) -> None:
    """Surface 4: TypeScript SDK (@mitos/sdk) via a node script the harness runs."""
    node = shutil.which("node")
    if not node:
        raise SkipSurface("node not on PATH; install Node 18+ to run the TS surface")
    sdk_dir = os.path.join(cfg.repo_root, "sdk", "typescript")
    # Resolve @mitos/sdk from the in repo TS package (it is the published shape).
    check = subprocess.run(
        [node, "-e", "require.resolve('@mitos/sdk')"],
        cwd=sdk_dir,
        capture_output=True,
        text=True,
    )
    if check.returncode != 0:
        raise SkipSurface(
            "@mitos/sdk not resolvable; run `npm ci && npm run build` in sdk/typescript "
            f"(node said: {check.stderr.strip()[:120]})"
        )
    script = _TS_SCRIPT
    env = dict(os.environ, MITOS_API_KEY=api_key, MITOS_BASE_URL=cfg.api_url)
    with tempfile.NamedTemporaryFile("w", suffix=".mjs", delete=False, dir=sdk_dir) as fh:
        fh.write(script)
        path = fh.name
    try:
        out = _run_json_subprocess([node, path], cwd=sdk_dir, env=env, timeout=180)
    finally:
        _safe(lambda: os.unlink(path))
    _assert_subprocess_quickstart(out, res, "ts")


def surface_go(cfg: Config, api_key: str, res: UserResult) -> None:
    """Surface 5: Go SDK (sdk/go) via a tiny program the harness builds and runs.

    The Go SDK has no run_code or files in scope, so this surface uses the
    documented create-template / fork / exec / terminate path. A local module
    replace points at the in repo sdk/go so `go run` needs no network.
    """
    go = shutil.which("go")
    if not go:
        raise SkipSurface("go not on PATH; install Go to run the Go SDK surface")
    sdk_dir = os.path.join(cfg.repo_root, "sdk", "go")
    if not os.path.isdir(sdk_dir):
        raise SkipSurface(f"sdk/go not found at {sdk_dir}")
    work = tempfile.mkdtemp(prefix="mitos-go-e2e-")
    try:
        _write(os.path.join(work, "main.go"), _GO_PROGRAM)
        gomod = _GO_MOD.format(sdk_path=sdk_dir)
        _write(os.path.join(work, "go.mod"), gomod)
        env = dict(
            os.environ,
            MITOS_API_KEY=api_key,
            MITOS_BASE_URL=cfg.api_url,
            GOFLAGS="-mod=mod",
        )
        # Resolve the dependency graph for the nested module offline against the
        # local replace. GOFLAGS=-mod=mod lets `go run` add the require line.
        out = _run_json_subprocess([go, "run", "."], cwd=work, env=env, timeout=300)
    finally:
        _safe(lambda: shutil.rmtree(work, ignore_errors=True))
    _assert_subprocess_quickstart(out, res, "go")


def surface_mcp(cfg: Config, api_key: str, res: UserResult) -> None:
    """Surface 6: MCP server (cmd/mitos-mcp) driven over stdio JSON-RPC.

    Builds the binary, then speaks newline delimited JSON-RPC 2.0: initialize,
    sandbox_create, sandbox_exec, sandbox_fork, terminate. The HTTP backend forks
    from a named TEMPLATE (it has no pools and no true fork-of-a-running-sandbox),
    so the fork call targets the template id, matching docs/mcp.md.
    """
    go = shutil.which("go")
    if not go:
        raise SkipSurface("go not on PATH; needed to build cmd/mitos-mcp")
    cmd_dir = os.path.join(cfg.repo_root, "cmd", "mitos-mcp")
    if not os.path.isdir(cmd_dir):
        raise SkipSurface(f"cmd/mitos-mcp not found at {cmd_dir}")
    work = tempfile.mkdtemp(prefix="mitos-mcp-e2e-")
    binpath = os.path.join(work, "mitos-mcp")
    try:
        build = subprocess.run(
            [go, "build", "-o", binpath, "./cmd/mitos-mcp"],
            cwd=cfg.repo_root,
            capture_output=True,
            text=True,
            timeout=300,
        )
        if build.returncode != 0:
            raise SkipSurface(f"go build cmd/mitos-mcp failed: {build.stderr.strip()[:200]}")

        env = dict(os.environ, MITOS_BASE_URL=cfg.api_url, MITOS_API_KEY=api_key)
        _run_mcp_quickstart(binpath, env, res)
    finally:
        _safe(lambda: shutil.rmtree(work, ignore_errors=True))


def surface_cli(cfg: Config, api_key: str, res: UserResult) -> None:
    """Surface 3: the mitos CLI sandbox verbs.

    HONEST NOTE: the mitos CLI sandbox verbs (create / exec / fork / ls /
    terminate) target a Kubernetes cluster resolved from kubeconfig, NOT the
    hosted SDK gateway via MITOS_API_KEY + MITOS_BASE_URL. See docs/cli.md
    ("Cluster backend"). There is no hosted-gateway CLI path today, so against
    prod this surface is SKIPPED with that reason rather than passed.

    When MITOS_E2E_CLI_KUBECONFIG points at a reachable cluster with the Mitos
    CRDs, the harness runs the real cluster path: create -> exec -> fork
    --replicas -> ls -> terminate. The fork flag is --replicas, not --count.
    """
    mitos_bin = shutil.which("mitos")
    if not mitos_bin:
        raise SkipSurface("mitos binary not on PATH; build with `go build -o mitos ./cmd/mitos/`")
    if not cfg.cli_kubeconfig:
        raise SkipSurface(
            "mitos CLI sandbox verbs are cluster-mode (kubeconfig), not the hosted "
            "MITOS_BASE_URL gateway; set MITOS_E2E_CLI_KUBECONFIG to a cluster to run them"
        )
    env = dict(os.environ, KUBECONFIG=cfg.cli_kubeconfig)
    pool = os.environ.get("MITOS_E2E_CLI_POOL", "dev-default")

    def run(args: list[str], step: str) -> str:
        p = subprocess.run(
            [mitos_bin, *args], env=env, capture_output=True, text=True, timeout=180
        )
        if p.returncode != 0:
            raise StepError(step, f"`mitos {' '.join(args)}` rc={p.returncode}: {p.stderr.strip()[:200]}")
        return p.stdout.strip()

    sid = ""
    forks: list[str] = []
    try:
        sid = run(["sandbox", "create", "--pool", pool], "create").splitlines()[-1].strip()
        res.sandbox_ids.append(sid)
        out = run(["sandbox", "exec", sid, "echo", "cli-hello"], "exec")
        if "cli-hello" not in out:
            raise StepError("exec", f"expected cli-hello, got {out!r}")
        fork_out = run(["sandbox", "fork", sid, "--replicas", str(FORK_COUNT)], "fork")
        forks = [ln.strip() for ln in fork_out.splitlines() if ln.strip()]
        if len(forks) != FORK_COUNT:
            raise StepError("fork", f"expected {FORK_COUNT} fork ids, got {forks!r}")
        res.sandbox_ids.extend(forks)
        for i, fid in enumerate(forks):
            fout = run(["sandbox", "exec", fid, "echo", f"clif-{i}"], "fork_exec")
            if f"clif-{i}" not in fout:
                raise StepError("fork_exec", f"fork {i}: got {fout!r}")
        run(["sandbox", "ls"], "ls")
    finally:
        for fid in forks:
            _safe(lambda fid=fid: subprocess.run(
                [mitos_bin, "sandbox", "terminate", fid], env=env, capture_output=True, timeout=60
            ))
        if sid:
            _safe(lambda: subprocess.run(
                [mitos_bin, "sandbox", "terminate", sid], env=env, capture_output=True, timeout=60
            ))


# ----------------------------------------------------------------------------
# Subprocess and MCP helpers
# ----------------------------------------------------------------------------


def _run_json_subprocess(
    argv: list[str], *, cwd: str, env: dict[str, str], timeout: float
) -> dict[str, Any]:
    """Run a subprocess that prints a single JSON object on its last stdout line.

    The TS and Go helper programs emit one JSON line with the quickstart outcome.
    A non-zero exit, or a missing JSON line, becomes a StepError.
    """
    try:
        p = subprocess.run(
            argv, cwd=cwd, env=env, capture_output=True, text=True, timeout=timeout
        )
    except subprocess.TimeoutExpired as e:
        raise StepError("subprocess", f"{argv[0]} timed out after {timeout}s") from e
    if p.returncode != 0:
        tail = (p.stderr.strip() or p.stdout.strip())[-400:]
        raise StepError("subprocess", f"{argv[0]} rc={p.returncode}: {tail}")
    line = ""
    for ln in reversed(p.stdout.strip().splitlines()):
        ln = ln.strip()
        if ln.startswith("{") and ln.endswith("}"):
            line = ln
            break
    if not line:
        raise StepError("subprocess", f"no JSON result line from {argv[0]}; stdout={p.stdout[-300:]!r}")
    try:
        return json.loads(line)
    except json.JSONDecodeError as e:
        raise StepError("subprocess", f"bad JSON from {argv[0]}: {e}") from e


def _assert_subprocess_quickstart(out: dict[str, Any], res: UserResult, tag: str) -> None:
    """Validate the JSON outcome emitted by a TS or Go helper program."""
    if not out.get("ok"):
        raise StepError(out.get("step", "subprocess"), out.get("error", "helper reported not ok"))
    sid = out.get("sandbox", "")
    forks = out.get("forks", [])
    if sid:
        res.sandbox_ids.append(sid)
    res.sandbox_ids.extend([f for f in forks if f])
    if len(forks) != FORK_COUNT:
        raise StepError("fork", f"{tag}: expected {FORK_COUNT} forks, got {len(forks)}")
    if not out.get("exec_ok"):
        raise StepError("exec", f"{tag}: exec assertion failed in helper")
    if not out.get("fork_exec_ok"):
        raise StepError("fork_exec", f"{tag}: fork exec assertion failed in helper")


def _run_mcp_quickstart(binpath: str, env: dict[str, str], res: UserResult) -> None:
    """Drive mitos-mcp over stdio JSON-RPC for the create / exec / fork flow."""
    proc = subprocess.Popen(
        [binpath],
        stdin=subprocess.PIPE,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        text=True,
        env=env,
        bufsize=1,
    )

    next_id = [0]

    def call(method: str, params: Optional[dict[str, Any]] = None) -> dict[str, Any]:
        next_id[0] += 1
        msg = {"jsonrpc": "2.0", "id": next_id[0], "method": method}
        if params is not None:
            msg["params"] = params
        assert proc.stdin is not None and proc.stdout is not None
        proc.stdin.write(json.dumps(msg) + "\n")
        proc.stdin.flush()
        line = proc.stdout.readline()
        if not line:
            err = proc.stderr.read() if proc.stderr else ""
            raise StepError(method, f"no response (server stderr: {err.strip()[:200]})")
        resp = json.loads(line)
        if "error" in resp and resp["error"]:
            raise StepError(method, f"jsonrpc error: {resp['error']}")
        return resp.get("result", {})

    def notify(method: str) -> None:
        assert proc.stdin is not None
        proc.stdin.write(json.dumps({"jsonrpc": "2.0", "method": method}) + "\n")
        proc.stdin.flush()

    def tool_text(result: dict[str, Any]) -> str:
        content = result.get("content", [])
        if not content:
            raise StepError("tools/call", f"tool result had no content: {result}")
        if result.get("isError"):
            raise StepError("tools/call", f"tool error: {content[0].get('text', '')[:200]}")
        return content[0].get("text", "")

    forks: list[str] = []
    created = ""
    try:
        init = call("initialize", {
            "protocolVersion": "2025-06-18",
            "capabilities": {},
            "clientInfo": {"name": "mitos-hosted-e2e", "version": "1.0.0"},
        })
        if not init.get("serverInfo"):
            raise StepError("initialize", f"no serverInfo in {init}")
        notify("notifications/initialized")

        create_res = call("tools/call", {"name": "sandbox_create", "arguments": {"pool": "python"}})
        text = tool_text(create_res)
        # "Created sandbox <id>"
        created = text.split()[-1].strip() if text else ""
        if not created:
            raise StepError("sandbox_create", f"could not parse sandbox id from {text!r}")
        res.sandbox_ids.append(created)

        exec_res = call("tools/call", {
            "name": "sandbox_exec",
            "arguments": {"sandbox": created, "command": "echo mcp-hello"},
        })
        exec_payload = json.loads(tool_text(exec_res))
        if "mcp-hello" not in exec_payload.get("stdout", ""):
            raise StepError("sandbox_exec", f"got {exec_payload!r}")

        # The HTTP backend forks from a named template, so fork the template id.
        fork_res = call("tools/call", {
            "name": "sandbox_fork",
            "arguments": {"sandbox": "python", "replicas": FORK_COUNT},
        })
        forks = json.loads(tool_text(fork_res))
        if not isinstance(forks, list) or len(forks) != FORK_COUNT:
            raise StepError("sandbox_fork", f"expected {FORK_COUNT} ids, got {forks!r}")
        res.sandbox_ids.extend(forks)
        for i, fid in enumerate(forks):
            fe = call("tools/call", {
                "name": "sandbox_exec",
                "arguments": {"sandbox": fid, "command": f"echo mcpf-{i}"},
            })
            fp = json.loads(tool_text(fe))
            if f"mcpf-{i}" not in fp.get("stdout", ""):
                raise StepError("fork_exec", f"fork {i}: got {fp!r}")
    finally:
        for fid in forks:
            _safe(lambda fid=fid: call("tools/call", {
                "name": "sandbox_terminate", "arguments": {"sandbox": fid},
            }))
        if created:
            _safe(lambda: call("tools/call", {
                "name": "sandbox_terminate", "arguments": {"sandbox": created},
            }))
        _safe(lambda: proc.stdin.close() if proc.stdin else None)
        _safe(proc.terminate)
        _safe(lambda: proc.wait(timeout=10))


def _safe(fn: Callable[[], Any]) -> None:
    """Run a cleanup callable, swallowing any error (best effort cleanup)."""
    try:
        fn()
    except Exception:
        pass


async def _asafe(fn: Callable[[], Any]) -> None:
    try:
        await fn()
    except Exception:
        pass


def _write(path: str, content: str) -> None:
    with open(path, "w", encoding="utf-8") as fh:
        fh.write(content)


# ----------------------------------------------------------------------------
# Embedded helper programs (TS and Go)
# ----------------------------------------------------------------------------

_TS_SCRIPT = r"""
// TypeScript SDK quickstart helper. Emits one JSON line with the outcome.
import { SandboxServer } from "@mitos/sdk";

const FORK_COUNT = 2;

async function main() {
  const out = { ok: false, step: "", sandbox: "", forks: [], exec_ok: false, fork_exec_ok: false, error: "" };
  let server, sandbox;
  const forks = [];
  try {
    server = new SandboxServer(); // MITOS_API_KEY + MITOS_BASE_URL from env
    out.step = "createTemplate";
    await server.createTemplate("python");
    out.step = "fork";
    sandbox = await server.fork("python");
    out.sandbox = sandbox.id;
    out.step = "exec";
    const r = await sandbox.exec("echo ts-hello");
    out.exec_ok = r.stdout.includes("ts-hello");
    out.step = "fork-children";
    let allForkExec = true;
    for (let i = 0; i < FORK_COUNT; i++) {
      const child = await server.fork("python");
      forks.push(child);
      out.forks.push(child.id);
      const fr = await child.exec(`echo tsf-${i}`);
      if (!fr.stdout.includes(`tsf-${i}`)) allForkExec = false;
    }
    out.fork_exec_ok = allForkExec;
    out.ok = out.exec_ok && out.fork_exec_ok && out.forks.length === FORK_COUNT;
  } catch (e) {
    out.error = String(e && e.message ? e.message : e);
  } finally {
    for (const f of forks) { try { await f.terminate(); } catch (e) {} }
    if (sandbox) { try { await sandbox.terminate(); } catch (e) {} }
    process.stdout.write("\n" + JSON.stringify(out) + "\n");
  }
}
main();
"""

_GO_MOD = """module mitoshostede2e

go 1.24

require github.com/mitos-run/mitos/sdk/go v0.0.0

replace github.com/mitos-run/mitos/sdk/go => {sdk_path}
"""

_GO_PROGRAM = r"""// Go SDK quickstart helper. Emits one JSON line with the outcome.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	mitos "github.com/mitos-run/mitos/sdk/go"
)

const forkCount = 2

type outcome struct {
	OK          bool     `json:"ok"`
	Step        string   `json:"step"`
	Sandbox     string   `json:"sandbox"`
	Forks       []string `json:"forks"`
	ExecOK      bool     `json:"exec_ok"`
	ForkExecOK  bool     `json:"fork_exec_ok"`
	Error       string   `json:"error"`
}

func emit(o outcome) {
	b, _ := json.Marshal(o)
	fmt.Printf("\n%s\n", string(b))
}

func main() {
	ctx := context.Background()
	o := outcome{Forks: []string{}}
	srv := mitos.NewSandboxServer() // base URL + token from env

	o.Step = "createTemplate"
	if _, err := srv.CreateTemplate(ctx, "python"); err != nil {
		o.Error = err.Error()
		emit(o)
		os.Exit(0)
	}
	o.Step = "fork"
	sb, err := srv.Fork(ctx, "python", "")
	if err != nil {
		o.Error = err.Error()
		emit(o)
		os.Exit(0)
	}
	o.Sandbox = sb.ID
	defer sb.Terminate(ctx)

	o.Step = "exec"
	res, err := sb.Exec(ctx, "echo go-hello")
	if err != nil {
		o.Error = err.Error()
		emit(o)
		os.Exit(0)
	}
	o.ExecOK = contains(res.Stdout, "go-hello")

	o.Step = "fork-children"
	forkExec := true
	var children []*mitos.Sandbox
	for i := 0; i < forkCount; i++ {
		child, err := srv.Fork(ctx, "python", "")
		if err != nil {
			o.Error = err.Error()
			emit(o)
			os.Exit(0)
		}
		children = append(children, child)
		o.Forks = append(o.Forks, child.ID)
		fr, err := child.Exec(ctx, fmt.Sprintf("echo gof-%d", i))
		if err != nil || !contains(fr.Stdout, fmt.Sprintf("gof-%d", i)) {
			forkExec = false
		}
	}
	o.ForkExecOK = forkExec
	o.OK = o.ExecOK && o.ForkExecOK && len(o.Forks) == forkCount

	for _, c := range children {
		_ = c.Terminate(ctx)
	}
	emit(o)
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
"""


# ----------------------------------------------------------------------------
# Surface registry and orchestration
# ----------------------------------------------------------------------------

SURFACES: list[tuple[str, str, Callable[[Config, str, UserResult], None]]] = [
    ("python-sync", "Python SDK sync (mitos.create / run_code / fork)", surface_python_sync),
    ("python-async", "Python SDK async (mitos.aio)", surface_python_async),
    ("cli", "mitos CLI sandbox verbs (cluster-mode)", surface_cli),
    ("typescript", "TypeScript SDK (@mitos/sdk SandboxServer)", surface_typescript),
    ("go", "Go SDK (sdk/go SandboxServer)", surface_go),
    ("mcp", "MCP server (cmd/mitos-mcp over stdio JSON-RPC)", surface_mcp),
    ("openai-agents", "OpenAI Agents adapter (mitos.integrations.openai_agents)", surface_openai_agents),
    ("langchain", "LangChain / deepagents adapter (mitos.integrations.langchain)", surface_langchain),
    ("python-node-template", "Python SDK, node template (create('node'))", surface_python_node_template),
    ("python-runcode-files", "Python SDK, files.write+read + run_code + fork", surface_python_runcode_files),
]

SURFACE_BY_NAME = {name: (desc, fn) for name, desc, fn in SURFACES}
SURFACE_ORDER = [name for name, _, _ in SURFACES]


def run_user(cfg: Config, n: int, surface: str, only_signup: bool, ts: int) -> UserResult:
    """Run one user end to end for a chosen surface."""
    email = f"u{n}-{ts}@{cfg.e2e_domain}"
    res = UserResult(n=n, surface=surface, email=email)
    try:
        outcome = run_signup_path(cfg, email, res)
        if only_signup:
            res.status = PASS
            return res
        desc, fn = SURFACE_BY_NAME[surface]
        fn(cfg, outcome.api_key, res)
        res.steps_done.append("quickstart(create+exec+fork)")
        res.status = PASS
    except SkipSurface as e:
        res.status = SKIPPED
        res.detail = str(e)
        res.failed_step = "quickstart"
    except StepError as e:
        res.status = FAIL
        res.failed_step = e.step
        res.detail = e.detail
    except Exception as e:  # defensive: never let one user crash the run
        res.status = FAIL
        res.failed_step = res.steps_done[-1] if res.steps_done else "unknown"
        res.detail = f"{type(e).__name__}: {e}"
    return res


def select_surfaces(args: argparse.Namespace) -> list[str]:
    """Resolve the surface list for the run from --surfaces and --users."""
    if args.surfaces:
        wanted = [s.strip() for s in args.surfaces.split(",") if s.strip()]
        unknown = [s for s in wanted if s not in SURFACE_BY_NAME]
        if unknown:
            raise SystemExit(
                f"unknown surfaces: {', '.join(unknown)}\n"
                f"valid surfaces: {', '.join(SURFACE_ORDER)}"
            )
        return wanted
    if args.users is not None:
        # First N surfaces in canonical order.
        return SURFACE_ORDER[: args.users]
    return list(SURFACE_ORDER)


# ----------------------------------------------------------------------------
# Output
# ----------------------------------------------------------------------------


def print_header(cfg: Config, surfaces: list[str], only_signup: bool) -> None:
    print("=" * 78)
    print("Mitos hosted end to end verification")
    print("=" * 78)
    print(f"  console base url : {cfg.base_url}")
    print(f"  sdk gateway url  : {cfg.api_url}")
    print(f"  e2e domain       : {cfg.e2e_domain}")
    print(f"  expected credit  : {cfg.expected_credit_cents} cents "
          f"(${cfg.expected_credit_cents / 100:.2f})")
    print(f"  bearer set       : {'yes' if cfg.e2e_token else 'NO (steps 2 to 4 will fail)'}")
    print(f"  mode             : {'signup only (steps 1 to 4)' if only_signup else 'full (steps 1 to 5)'}")
    print(f"  users / surfaces : {len(surfaces)} -> {', '.join(surfaces)}")
    print("=" * 78)
    print()


def print_user_line(res: UserResult) -> None:
    tag = {PASS: "PASS   ", FAIL: "FAIL   ", SKIPPED: "SKIPPED"}[res.status]
    print(f"[{tag}] user {res.n:<2} {res.surface:<22} {res.email}")
    if res.steps_done:
        print(f"           steps: {' -> '.join(res.steps_done)}")
    if res.status == FAIL:
        print(f"           FAILED at step '{res.failed_step}': {res.detail}")
    elif res.status == SKIPPED:
        print(f"           skipped: {res.detail}")
    print()


def print_summary(results: list[UserResult]) -> int:
    print("=" * 78)
    print("Summary")
    print("=" * 78)
    print(f"  {'user':<5} {'surface':<22} {'status':<8} {'detail'}")
    print(f"  {'-' * 4:<5} {'-' * 21:<22} {'-' * 7:<8} {'-' * 30}")
    n_pass = n_fail = n_skip = 0
    for r in results:
        if r.status == PASS:
            n_pass += 1
            detail = (
                f"credit {r.credit_cents}c"
                + (f", key ...{r.api_key_tail}" if r.api_key_tail else "")
            )
        elif r.status == FAIL:
            n_fail += 1
            detail = f"at {r.failed_step}: {r.detail}"
        else:
            n_skip += 1
            detail = r.detail
        print(f"  {r.n:<5} {r.surface:<22} {r.status:<8} {detail[:60]}")
    print()
    print(f"  total {len(results)}: {n_pass} passed, {n_fail} failed, {n_skip} skipped")
    print("=" * 78)
    return n_fail


# ----------------------------------------------------------------------------
# Main
# ----------------------------------------------------------------------------


def build_parser() -> argparse.ArgumentParser:
    p = argparse.ArgumentParser(
        description="Verify the hosted Mitos signup, credit, and quickstart flow.",
        formatter_class=argparse.RawDescriptionHelpFormatter,
        epilog=(
            "surfaces:\n  " + "\n  ".join(f"{n:<22} {d}" for n, d, _ in SURFACES)
        ),
    )
    p.add_argument(
        "--only-signup",
        action="store_true",
        help="run steps 1 to 4 only (signup, token, verify, credit); no SDK or KVM needed",
    )
    p.add_argument(
        "--users",
        type=int,
        default=None,
        help="run the first N surfaces in canonical order (one user each)",
    )
    p.add_argument(
        "--surfaces",
        type=str,
        default=None,
        help="comma separated surface names to run (overrides --users)",
    )
    p.add_argument(
        "--list-surfaces",
        action="store_true",
        help="print the surface names and exit",
    )
    return p


def main(argv: Optional[list[str]] = None) -> int:
    args = build_parser().parse_args(argv)
    if args.list_surfaces:
        for name, desc, _ in SURFACES:
            print(f"{name:<22} {desc}")
        return 0

    cfg = Config.from_env()
    surfaces = select_surfaces(args)
    ts = int(time.time())

    print_header(cfg, surfaces, args.only_signup)

    results: list[UserResult] = []
    for i, surface in enumerate(surfaces, start=1):
        res = run_user(cfg, i, surface, args.only_signup, ts)
        results.append(res)
        print_user_line(res)

    n_fail = print_summary(results)
    return 1 if n_fail > 0 else 0


if __name__ == "__main__":
    sys.exit(main())
