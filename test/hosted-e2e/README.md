# Hosted end to end verification harness

`verify_hosted.py` proves a brand new user can use the HOSTED Mitos service
exactly as the docs describe: sign up, receive the free signup credit, and run
the quickstart to create and FORK a sandbox, across a set of user variations
(different SDKs, adapters, and templates).

It is the acceptance artifact for the hosted launch journey. Every user prints
PASS, FAIL, or SKIPPED with the failing step and the error, and the process exits
non-zero if any selected user fails.

## The two halves

The flow splits so the cheap half can run before the cluster exists.

1. Steps 1 to 4, the signup and credit path, hit the console and onboarding HTTP
   API only. They use the gated QA token seam (`GET /onboarding/e2e/token`, the
   PR #515 endpoint) so the harness can drive verification without reading a
   mailbox. No KVM needed. Run them with `--only-signup`.

2. Step 5, the quickstart, drives a chosen surface against the SDK gateway: it
   creates a sandbox, execs, FORKS into siblings, execs in each fork, asserts the
   outputs, and terminates. This half needs the live KVM cluster behind the
   gateway.

## Per user flow

Each user gets a unique email `u<N>-<timestamp>@<MITOS_E2E_DOMAIN>`.

1. `POST {MITOS_BASE_URL}/onboarding/signup {"email": <email>}` expects 202.
2. `GET {MITOS_BASE_URL}/onboarding/e2e/token?email=<email>` with
   `Authorization: Bearer {MITOS_E2E_TOKEN}` expects 200 `{"token": <raw>}`.
3. `POST {MITOS_BASE_URL}/onboarding/verify {"token": <raw>}` expects 200 with an
   `apiKey` (`sk-...`) and a `Set-Cookie: mitos_session` cookie.
4. `GET {MITOS_BASE_URL}/console/billing` with the `mitos_session` cookie expects
   200 with `balance_cents` equal to the expected credit (default 500, the $5
   grant). Asserted.
5. Quickstart: with the `apiKey` as `MITOS_API_KEY` and the gateway as
   `MITOS_BASE_URL`, run the surface specific create, exec, fork, and terminate.

## Configuration (environment)

| Variable | Default | Meaning |
| --- | --- | --- |
| `MITOS_BASE_URL` | `https://mitos.run` | Console and onboarding origin (steps 1 to 4). |
| `MITOS_API_URL` | `https://api.mitos.run` | SDK gateway origin (step 5 create / exec / fork). |
| `MITOS_E2E_TOKEN` | (empty) | Bearer for the gated QA token seam. Required for steps 2 to 4. |
| `MITOS_E2E_DOMAIN` | `e2e.mitos.run` | Email domain; must match the console allowlist. |
| `MITOS_E2E_EXPECTED_CREDIT_CENTS` | `500` | Asserted `balance_cents` after signup. |
| `MITOS_E2E_HTTP_TIMEOUT` | `30` | Per request HTTP timeout in seconds. |
| `MITOS_E2E_CLI_KUBECONFIG` | (empty) | Kubeconfig for the cluster-mode CLI surface (see below). |
| `MITOS_E2E_CLI_POOL` | `dev-default` | SandboxPool the CLI surface claims from. |

Note: the docs default the SDK base URL to `https://mitos.run`. This harness
splits the SDK gateway out as `MITOS_API_URL` (`api.mitos.run`) so each half can
be pointed independently. If your deployment serves both from one origin, set
both variables to the same value.

The server side seam these steps target is mounted only when the console runs
with `MITOS_CONSOLE_E2E=1`, `MITOS_CONSOLE_E2E_TOKEN=<bearer>`, and
`MITOS_CONSOLE_E2E_DOMAIN=<domain>` (see `cmd/console/onboarding.go`). The
harness `MITOS_E2E_TOKEN` must equal the server `MITOS_CONSOLE_E2E_TOKEN`, and
`MITOS_E2E_DOMAIN` must equal `MITOS_CONSOLE_E2E_DOMAIN`.

## Usage

```bash
# List the surfaces.
python3 verify_hosted.py --list-surfaces

# Signup and credit path only (no SDK, no KVM). Run this FIRST.
MITOS_BASE_URL=https://mitos.run \
MITOS_E2E_TOKEN=... \
MITOS_E2E_DOMAIN=e2e.mitos.run \
python3 verify_hosted.py --only-signup --users 10

# Full run against prod (needs the live gateway and KVM cluster).
MITOS_BASE_URL=https://mitos.run \
MITOS_API_URL=https://api.mitos.run \
MITOS_E2E_TOKEN=... \
MITOS_E2E_DOMAIN=e2e.mitos.run \
python3 verify_hosted.py

# A subset of surfaces.
python3 verify_hosted.py --surfaces python-sync,typescript,mcp

# The first N surfaces in canonical order.
python3 verify_hosted.py --users 4
```

Exit code is 0 only when no selected user failed. SKIPPED does not fail the run,
but it is reported loudly with the reason so a missing dependency never reads as
a pass.

## The surfaces

| Surface | What it drives | Needs |
| --- | --- | --- |
| `python-sync` | `mitos.create("python")`, `run_code`, `exec`, `fork(2)`, exec in forks | `pip install mitos-run` |
| `python-async` | `mitos.aio.create`, async `exec` / `run_code` / `fork` | `pip install mitos-run` |
| `cli` | `mitos sandbox create / exec / fork --replicas / ls / terminate` | `mitos` binary plus a cluster (see note) |
| `typescript` | `@mitos/sdk` `SandboxServer.fork` / `exec`, via a node script | `node` plus a built `@mitos/sdk` |
| `go` | `sdk/go` `SandboxServer` create / fork / exec, via a tiny built program | `go` toolchain |
| `mcp` | `cmd/mitos-mcp` over stdio JSON-RPC: `sandbox_create` / `sandbox_exec` / `sandbox_fork` | `go` toolchain |
| `openai-agents` | `mitos.integrations.openai_agents.MitosSandboxTools`, plus native fork | `pip install mitos-run` |
| `langchain` | `mitos.integrations.langchain.MitosSandbox`, plus native fork | `pip install mitos-run` |
| `python-node-template` | `mitos.create("node")`, node exec, `fork(2)` | `pip install mitos-run` |
| `python-runcode-files` | `files.write` + `files.read` + `run_code`, then `fork(2)` reads pre-fork state | `pip install mitos-run` |

### Honest surface notes

- `cli`: the `mitos` CLI sandbox verbs target a Kubernetes cluster resolved from
  kubeconfig, NOT the hosted SDK gateway via `MITOS_API_KEY` + `MITOS_BASE_URL`
  (see `docs/cli.md`, "Cluster backend"). There is no hosted-gateway CLI path
  today, so against prod this surface is SKIPPED with that reason. Set
  `MITOS_E2E_CLI_KUBECONFIG` to a reachable cluster with the Mitos CRDs to run
  the real cluster path. The fork flag is `--replicas`, not `--count`.
- `mcp`: the MCP HTTP backend forks from a named TEMPLATE; it has no pools and no
  true fork-of-a-running-sandbox (see `docs/mcp.md`), so the fork call targets the
  template id `python`. `sandbox_create` returns the text `Created sandbox <id>`,
  which the harness parses for the id.
- `go`: the Go SDK has no `run_code` or files in scope, so the Go surface uses the
  documented create-template / fork / exec / terminate path. A local module
  `replace` points at the in repo `sdk/go`, so `go run` needs no network.
- `openai-agents` and `langchain`: the adapters are usable without the framework
  package installed (only `as_function_tools` / deepagents need it), so these
  surfaces need only `mitos-run`. Fork is reached as the native op.
- `typescript`: resolve `@mitos/sdk` from the in repo `sdk/typescript` by running
  `npm ci && npm run build` there first; otherwise the surface is SKIPPED with
  that instruction.

## What is validated in this PR

The hosted service is not live yet and there is no local KVM, so the full step 5
quickstart cannot run here. Validated:

- `python3 -m py_compile verify_hosted.py` passes; no em or en dashes anywhere.
- Steps 1 to 4 smoke green against a stdlib mock that reproduces the documented
  wire shapes from `internal/saas/onboarding/http.go`, `e2e.go`, and
  `console/console.go`. PASS, FAIL (non-zero exit), and SKIPPED were all
  exercised.
- The embedded Go helper compiles against the real local `sdk/go` module.
- The embedded TypeScript helper passes `node --check`.
- `cmd/mitos-mcp` builds.

The moment the console is live with the e2e seam, `--only-signup` runs as is. The
moment the gateway and KVM cluster are live, the full run does too.
