# Quickstart

Signup to a first successful `run_code` in minutes, no card on the free tier,
one SDK package, one snippet that works first try.

## One package, one snippet

```bash
pip install mitos-run
```

The PyPI distribution is named `mitos-run`, but the import package stays
`mitos`: you `pip install mitos-run` and `import mitos`.

```python
import mitos

# Set MITOS_API_KEY (get a key from https://mitos.run). The base URL defaults to
# the hosted production endpoint https://mitos.run, so no base URL is needed.
sb = mitos.create("python")                 # Ready sandbox handle
print(sb.run_code("print(1 + 1)").text)     # 2
sb.terminate()
```

That is the whole thing: one `pip install mitos-run`, one import, one `create`, code
execution, no second SDK to install. `mitos.create(image, api_key=..., base_url=...)`
resolves the API key (argument, else `MITOS_API_KEY`) and the base URL (argument,
else `MITOS_BASE_URL`, else the hosted endpoint `https://mitos.run`), then returns
a sandbox handle that exposes `exec`, `run_code`, `files`, `fork`, and `terminate`
directly. The API key value is never logged. `Sandbox.create(...)` is an alias for
the same call.

## Local or self-hosted standalone

To target a local standalone `sandbox-server` or a self-hosted cluster instead of
the hosted endpoint, set the base URL (argument or `MITOS_BASE_URL`). The
standalone server runs tokenless, so no key is required:

```python
import mitos

sb = mitos.create("python", base_url="http://localhost:8080")
print(sb.run_code("print(1 + 1)").text)     # 2
sb.terminate()
```

## The full flat handle

```python
import mitos

sb = mitos.create("python", api_key="sk-...")

# Files, stateful code, and fork all work on the same flat handle.
sb.files.write("/workspace/plan.txt", "draft")
print(sb.files.read("/workspace/plan.txt"))      # draft

ex = sb.run_code("import math; math.sqrt(144)")
print(ex.text)                                   # 12.0

# Fork the sandbox into independent siblings to try two approaches at once.
fork_a, fork_b = sb.fork(2)

sb.terminate()
```

The async client mirrors the flat path: `await mitos.aio.create("python")`
returns an async handle with the same `exec` / `run_code` / `files` / `fork` /
`terminate` surface.

## Where the key comes from

The hosted endpoint is the default, so the headline quickstart needs only an API
key. The standalone `sandbox-server` runs tokenless and ignores the key, so the
local variant above works against a local server with no signup at all. The
hosted control plane verifies the same `Authorization: Bearer` header
server-side, and you get a key from
the self-serve onboarding funnel: sign up with an email, click the verification
link, and your Personal organization, free-tier signup credit, and first API key
are provisioned automatically. See [docs/saas/onboarding.md](saas/onboarding.md)
for the funnel, the free-credit grant, and the current availability mode
(waitlist vs open self-serve).
