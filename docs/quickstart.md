# Quickstart

Signup to a first successful `run_code` in minutes, no card on the free tier,
one SDK package, one snippet that works first try. This is the canonical
quickstart; the README headline points here.

## One package, one snippet

```bash
pip install mitos
```

```python
import mitos

# MITOS_API_KEY and MITOS_BASE_URL come from the environment (explicit args override).
sb = mitos.create("python")                 # Ready sandbox handle
print(sb.run_code("print(1 + 1)").text)     # 2
sb.terminate()
```

That is the whole thing: one `pip install mitos`, one import, one `create`, code
execution, no second SDK to install. `mitos.create(image, api_key=..., base_url=...)`
resolves the API key (argument, else `MITOS_API_KEY`) and the base URL (argument,
else `MITOS_BASE_URL`), then returns a sandbox handle that exposes `exec`,
`run_code`, `files`, `fork`, and `terminate` directly. The API key value is never
logged. `Sandbox.create(...)` is an alias for the same call.

## The full flat handle

```python
import mitos

sb = mitos.create("python", api_key="sk-...", base_url="http://localhost:8080")

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

The standalone `sandbox-server` runs tokenless and ignores the key, so the
quickstart works against a local server with no signup at all. The hosted
control plane verifies the same `Authorization: Bearer` header server-side
([#210](https://github.com/mitos-run/mitos/issues/210)), and you get a key from
the self-serve onboarding funnel: sign up with an email, click the verification
link, and your Personal organization, free-tier signup credit, and first API key
are provisioned automatically. See [docs/saas/onboarding.md](saas/onboarding.md)
for the funnel, the free-credit grant, and the current availability mode
(waitlist vs open self-serve).
