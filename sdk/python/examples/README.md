# Mitos Python SDK examples

Runnable, copy-paste examples for the Mitos Python SDK. Each file is import-safe
(all real work is behind a `if __name__ == "__main__":` guard) and is gated by
the `sdk-examples` CI job, which byte-compiles every file here and imports it so a
renamed or removed SDK symbol fails the build.

## Two paths, one SDK

- Flat / direct path (`mitos.create(...)`): the standalone sandbox-server or the
  hosted control plane. No Kubernetes required. Auth and base URL resolve from
  `MITOS_API_KEY` and `MITOS_BASE_URL` (the hosted default is `https://mitos.run`
  when `MITOS_BASE_URL` is unset).
- Operator path (`mitos.AgentRun(...)`): the Kubernetes operator, for durable
  Workspaces and the `{git}` rendezvous push. Needs a cluster and `KUBECONFIG`.

Install the SDK:

```
pip install mitos-run        # import name stays `mitos`
```

## Use-case examples

| File | Use case | One-line hook | Path |
| --- | --- | --- | --- |
| `code_interpreter.py` | Code interpreter | A stateful `run_code` data-analysis loop that returns a chart. | direct |
| `best_of_n.py` | Best-of-N | Fork one warm sandbox into N, run attempts in parallel, keep the winner. | direct |
| `rl_evals.py` | RL / evals | A SWE-bench-style harness that forks many environments from one golden state. | direct |
| `background_agent.py` | Background agent | Task in, PR out: a durable workspace pushed to a rendezvous git remote on terminate. | operator |

Other examples in this directory: `quickstart.py` (the README hero),
`direct.py` (the minimal standalone smoke test, executed end to end on real KVM
in CI), and `browser_cdp.py` (drive a headless Chromium sandbox over CDP).

### code_interpreter.py

Hand an agent a sandboxed Python kernel, feed it cells, and get back stdout plus
rich display artifacts (last-expression value, a PNG chart). State persists
across `run_code` calls for the sandbox lifetime. Needs a base image carrying the
code-interpreter kernel (pandas + matplotlib for the plotting cell).

```
export MITOS_API_KEY=sk-...            # hosted
python3 code_interpreter.py
# or, standalone (run a sandbox-server first):
python3 code_interpreter.py http://localhost:8080
```

### best_of_n.py

One warm parent, N copy-on-write siblings via `source.fork(N)`. Each sibling is
isolated; the example runs an attempt in each concurrently, scores them, and
keeps the winner.

```
export MITOS_API_KEY=sk-...
python3 best_of_n.py
# or: python3 best_of_n.py http://localhost:8080
```

### rl_evals.py

Set up one golden environment, fork it once per task instance so every candidate
runs against a byte-identical start, run the grader in each fork, and report the
aggregate pass-rate. The bundled instances are a self-contained toy grader; swap
the setup, candidate, and grader strings for a real benchmark.

```
export MITOS_API_KEY=sk-...
python3 rl_evals.py
# or: python3 rl_evals.py http://localhost:8080
```

### background_agent.py

Operator path. Binds a sandbox to a durable Workspace, lets it edit a repo under
`/workspace`, and on terminate pushes the repo paths to a rendezvous git remote
on a per-attempt branch via the `{git}` output. A human or CI opens the PR from
that branch; git is the merge layer.

Requirements: a reachable cluster with the Mitos CRDs, `KUBECONFIG` set, and a
rendezvous remote in `MITOS_GIT_REMOTE`. On the husk warm-pool path the push is
best-effort today; see `docs/workspaces.md` for the fully wired path and for
authenticating a push to an external remote.

```
export KUBECONFIG=~/.kube/config
export MITOS_GIT_REMOTE=https://git.example.com/org/repo.git
python3 background_agent.py
```

Honest gap: the SDK has no first-class helper for `spec.git.paths` yet, so this
example sets it by patching the Workspace CRD through the Kubernetes client
`AgentRun` already loaded. A `spec.git` helper is a follow-up.

## What CI verifies (and what it does not)

The `sdk-examples` job byte-compiles and import-checks every file here on each
PR. Import runs each example's module-level code (imports plus the drift-guard
asserts), so a renamed SDK symbol fails the build. The examples are NOT executed
in that job: `run_code`, `exec`, fork, and the `{git}` push all need a guest VM
the job does not boot, so end-to-end execution is proven separately on real KVM
(`examples/direct.py` in the kvm-test job). Run the examples above yourself
against the hosted service or a standalone sandbox-server to see them end to end.
