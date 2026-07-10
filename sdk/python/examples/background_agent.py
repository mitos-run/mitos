"""Background agent: task in, PR out, on a durable workspace via the {git} output.

Use case hook: kick off a long-running agent against a durable, forkable
Workspace, let it edit a repo checked out under /workspace, and on terminate have
the controller push the repo paths to a rendezvous git remote on a per-attempt
branch. A human or CI then opens the PR from that branch. Git is the merge layer;
the engine only ever pushes a branch, it never merges.

This rides the Kubernetes operator path (AgentRun), not the flat direct path,
because durable Workspaces and the {git} rendezvous push are cluster features:

    agent = mitos.AgentRun(namespace="default")
    ws = agent.create_workspace("feature-x")
    sb = agent.sandbox(image="python", workspace="feature-x", ready=True)
    sb.exec("... edit /workspace/repo ...")
    sb.terminate(outputs=[{"git": {"remote": REMOTE, "branch": "attempt/{{.name}}"}}])

How the pieces fit (see docs/workspaces.md):
  - A bound sandbox hydrates the workspace head into /workspace on start and
    dehydrates a new committed WorkspaceRevision on terminate.
  - A {git} output pushes the workspace ``spec.git.paths`` content to ``remote``
    on a branch rendered from the ``{{.name}}`` (claim name) template, defaulting
    to ``attempt/<name>``.
  - A {git} output on a workspace with no ``spec.git.paths`` is a no-op, so this
    example declares ``spec.git.paths`` on the workspace before running, using the
    first-class ``mitos.git`` helper (issue #619).
  - On the husk warm-pool path the push is currently BEST-EFFORT (logged and
    skipped if the node-CAS read is not wired); on the raw forkd path it is fully
    wired. Authenticating a push to an external remote needs a credentials Secret;
    pass it declaratively with ``mitos.git(paths=[...], credentials_secret=(name,
    key))``. See docs/workspaces.md.

Requirements to run end to end: a reachable cluster with the mitos CRDs and a
SandboxPool (or the default-pool path), KUBECONFIG set, and a rendezvous remote
in MITOS_GIT_REMOTE (a bare repo URL the controller can push to). Without those
this is illustrative; it is byte-compiled and import-checked by the sdk-examples
CI job, and the asserts below run at import time as a real API-surface check.

Run::

    export KUBECONFIG=~/.kube/config
    export MITOS_GIT_REMOTE=https://git.example.com/org/repo.git
    python3 background_agent.py
"""

import os
import uuid

import mitos
from mitos.client import AgentRun
from mitos.sandbox import Sandbox
from mitos.workspace import Workspace

# Drift guard: attribute access on the classes does not import kubernetes (it is
# an optional, lazily loaded extra), so this stays safe in the import-check job
# while still failing if a method the example relies on is renamed or dropped.
assert hasattr(mitos, "AgentRun")
assert hasattr(mitos, "git")
assert all(hasattr(AgentRun, m) for m in ("sandbox", "create_workspace", "workspace"))
assert all(hasattr(Sandbox, m) for m in ("exec", "terminate"))
assert all(hasattr(Workspace, m) for m in ("log", "diff", "set_git"))

# The git helper is pure spec construction, so exercise it at import time as a
# real API-surface check: it must build the spec.git.paths this example relies on.
assert mitos.git(paths=["/workspace/repo"]).to_spec() == {"paths": ["/workspace/repo"]}


# The task the background agent performs inside /workspace/repo. A real agent
# would run a model here; this writes one file and stages it so there is a repo
# diff to push.
TASK = """
set -e
mkdir -p /workspace/repo
cd /workspace/repo
git init -q 2>/dev/null || true
printf 'print("hello from the background agent")\\n' > app.py
git add app.py
git -c user.email=agent@mitos.run -c user.name=agent commit -q -m 'add app.py' || true
echo wrote /workspace/repo/app.py
"""


def main() -> None:
    remote = os.environ.get("MITOS_GIT_REMOTE")
    if not remote:
        raise SystemExit(
            "set MITOS_GIT_REMOTE to a rendezvous git remote the controller can "
            "push to (a bare repo URL); see docs/workspaces.md for auth."
        )

    agent = mitos.AgentRun(namespace=os.environ.get("MITOS_NAMESPACE", "default"))
    ws_name = f"bg-{uuid.uuid4().hex[:8]}"

    # Declare the repo paths that get history and the rendezvous push
    # declaratively with the first-class spec.git helper (issue #619), so the
    # workspace is created with spec.git.paths already set. To push to an external
    # remote, add credentials_secret=("my-secret", "token").
    agent.create_workspace(ws_name, git=mitos.git(paths=["/workspace/repo"]))

    # Bind a sandbox to the workspace; ready=True blocks until it is Ready so the
    # task does not race the hydrate. image= uses the lazy default-pool path.
    sb = agent.sandbox(image="python", workspace=ws_name, ready=True)
    result = sb.exec(TASK, timeout=120)
    print(result.stdout, end="")
    if result.exit_code != 0:
        raise SystemExit(f"task FAILED: exit {result.exit_code} stderr={result.stderr!r}")

    # Terminate with a {git} output: the controller dehydrates /workspace into a
    # new committed revision, then pushes spec.git.paths to the rendezvous remote
    # on a per-attempt branch. The returned name is the workspace; its revisions
    # (and gitPushes) are discoverable with agent.workspace(ws_name).log().
    returned = sb.terminate(
        outputs=[{"git": {"remote": remote, "branch": "attempt/{{.name}}"}}],
    )
    print(f"terminated; workspace={returned}")
    print(f"open a PR from branch attempt/<claim-name> on {remote}")
    print("background_agent example OK")


if __name__ == "__main__":
    main()
