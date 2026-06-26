# Recipe: serve a durable workspace as a ready dev-environment URL

`mitos workspace serve` turns a durable `Workspace` into a live, URL-addressable
dev environment: it warm-claims a forked sandbox from a pool, binds it to the
workspace so the filesystem state is hydrated on start, sets the expose route,
and returns a URL your team (or an agent) can open immediately.

This is the Mitos answer to Coder and Daytona: instead of managing long-running
VMs, you snapshot the environment once, keep a pool of warm forks, and claim a
fresh fork each time someone needs a new session. The warm-claim latency is
benchmarked in `bench/` (see the husk activation latency bench).

## What ships today

- `mitos workspace serve <ws> --pool P [--port N] [--sharing private|link|org|authenticated|public] [--as L] [--expose-domain D]`
  warm-claims a forked sandbox, binds it to the workspace (`spec.workspaceRef`),
  sets `spec.expose{port, label, sharing}`, waits for the sandbox to reach
  `Ready`, and prints the URL `https://<label>.<expose-domain>/`.
- The command prints an honest note: the URL is reachable once the expose proxy
  is deployed, `*.<expose-domain>` DNS resolves to it, and (for the `private`
  default sharing tier) the caller has completed OIDC login.
- Default sharing is `private` (OIDC-gated, no token in the URL).
  `--sharing link` produces a signed-URL tier; the link-token minting flow in
  the cluster path is a follow-up item.
- Go SDK: `ws.Serve(ctx, opts...)` returns a `*ServedWorkspace` with a `.URL`
  field. The other five SDKs (Python, TypeScript, Ruby, Rust, Java) are coming
  in a follow-up release.

## Prerequisites

1. **A Workspace.** A `Workspace` (and at least one committed `WorkspaceRevision`)
   created with `mitos workspace create` or the Go SDK. See
   `docs/workspaces.md` for the full lifecycle.

2. **A SandboxPool.** A `SandboxPool` with at least `minWarm: 1` that has your
   dev environment baked into the snapshot. The pool image should have the
   language runtime, tools, and project dependencies installed so the claim is
   warm.

   ```yaml
   apiVersion: mitos.run/v1
   kind: SandboxPool
   metadata:
     name: python
   spec:
     template:
       image: python:3.12-slim
       init: ["pip install -r /workspace/requirements.txt"]
       resources: { cpu: "2", memory: "2Gi" }
       volumes:
         - { name: workspace, size: 10Gi, forkPolicy: Snapshot }
     warm: { min: 3 }
   ```

3. **The expose proxy deployed.** The Helm chart must have `expose.enabled: true`
   with `expose.domain` set to your expose domain, a wildcard TLS certificate for
   `*.<expose-domain>` mounted, and `*.<expose-domain>` DNS pointing to the proxy.
   See `docs/preview-urls.md` for Helm wiring and `docs/preview-urls.md#tls` for
   certificate options.

4. **OIDC configured (for the `private` default).** When `--sharing private`
   (the default) is in effect, callers authenticate through the OIDC flow at
   `auth.<expose-domain>`. Set `expose.oidc.issuer` and `expose.oidc.clientID`
   in the Helm values. Without this, the URL resolves but the proxy returns 401.

## CLI walkthrough

```bash
# 1. Create a workspace (or use an existing one).
mitos workspace create my-code

# 2. Serve it: claim a warm fork, bind the workspace, and get the URL.
mitos workspace serve my-code --pool python --expose-domain mitos.app
```

The command prints something like:

```
https://ws-abc123.mitos.app/

Note: reachable once the expose proxy is deployed, *.mitos.app DNS
resolves to it, and (for sharing=private) OIDC login is complete.
```

Optional flags:

| Flag | Default | Meaning |
|---|---|---|
| `--pool P` | (required) | The SandboxPool to warm-claim from |
| `--port N` | `8080` | The guest port the proxy routes traffic to |
| `--sharing S` | `private` | Access tier: private, link, org, authenticated, public |
| `--as L` | generated | A fixed single-DNS-label for the subdomain |
| `--expose-domain D` | `$MITOS_EXPOSE_DOMAIN` | The operator-configured expose domain |

## Go SDK

```go
import (
    "context"
    "fmt"
    "log"

    mitos "github.com/mitos-run/mitos/sdk/go"
)

func main() {
    client, err := mitos.NewClient()
    if err != nil {
        log.Fatal(err)
    }

    ws, err := client.Workspaces().Get(context.Background(), "my-code")
    if err != nil {
        log.Fatal(err)
    }

    served, err := ws.Serve(context.Background(),
        mitos.WithServePool("python"),
        mitos.WithServeExposeDomain("mitos.app"),
    )
    if err != nil {
        log.Fatal(err)
    }

    fmt.Println(served.URL)
    // https://ws-abc123.mitos.app/
}
```

`*ServedWorkspace` carries `.URL`, the sandbox ID, and the sharing tier. The
workspace is hydrated into `/workspace` inside the sandbox on claim via the
content-addressed store.

## The warm-fork angle

The distinguishing property: one `SandboxPool` keeps N warm microVMs pre-snapshotted
with your environment installed. When `serve` is called, the controller picks a
warm husk, forks it (copy-on-write), and binds the workspace filesystem in under
a second. Each fork is an independent Firecracker microVM; each gets its own
unique label and its own URL:

```
pool: 3 warm husks
  ->  call serve 3x
  ->  3 independent sandboxes, each reachable at its own https://<label>.mitos.app/
```

Each child's URL routes only to that child's process state. A fork is a fresh
sandbox; in-flight HTTP sessions from a previous session do not carry over, and
each child writes its own workspace revision on terminate.

## Honest status and deferred items

The following items are intentionally deferred to later slices:

- **Other SDKs.** Python, TypeScript, Ruby, Rust, and Java SDK support for
  `workspace serve` is coming in a follow-up release. Only the Go SDK ships
  today.
- **Link-token minting in the cluster path.** The `--sharing link` flag sets
  the sharing tier on the CRD, but the signed-URL minting flow through the
  controller path is a follow-up item. The `private` default is fully
  operational.
- **Label uniqueness enforcement.** The route table keys by `label`, and labels
  must be globally unique. The current implementation is operator-owned: if two
  sandboxes declare the same `spec.expose.label` they collide non-deterministically.
  A global label-allocation registry with reserved-name enforcement across tenants
  is a later slice. See `docs/preview-urls.md` for the full description of this
  limitation.
- **Memory-snapshot resumable heads.** Resuming a paused workspace from a memory
  snapshot (rather than hydrating from the content-addressed store on each claim)
  is a separate roadmap item.

## Follow-ups in the expose stack

Before serving production traffic, review `docs/preview-urls.md` for the
production gate: the expose ingress adds a public attack surface and is not
cleared for untrusted tenants until the external security review and the
abuse-control envelope are complete.
