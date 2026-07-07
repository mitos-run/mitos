# Building templates from images

A pool template snapshot is a paused, booted Firecracker microVM captured to
disk. Forks restore it copy-on-write. This document describes how the engine
turns a `SandboxPool.spec.template.image` into that snapshot on a real (KVM) node,
the image-vs-file heuristic, the agent-binary requirement, and what init
commands mean.

## The pipeline

When the pool reconciler needs a snapshot for a template it calls the forkd
`CreateTemplate` RPC with the template id, the image, and the template's init
commands. On the real engine (`internal/fork`) `CreateTemplate` does:

1. Pull. If the image is an OCI reference, `internal/ociroot.PullImage`
   anonymously pulls it from the registry (the keychain still applies for
   configured private registries). This is the only network step.
2. Flatten. `ociroot.ExtractImage` runs the image's layers through
   go-containerregistry's `mutate.Extract` and untars the flattened tree into a
   temp directory, preserving modes and symlinks. The extractor is hardened
   against path traversal: any entry that would escape the destination
   directory, via `..` components or an absolute/escaping symlink target, is
   rejected, because image tars are untrusted input.
3. Inject the agent. `ociroot.InjectAgent` copies the guest agent binary to
   `/init` (mode 0755), ensures a `/bin/sh` exists (using the injected static
   busybox if the image ships no shell), and creates the mount points the agent
   needs (`/proc`, `/sys`, `/dev`, `/tmp`, `/run`, `/workspace`). The agent is
   PID 1 in the booted VM.
4. Build the ext4. `ociroot.BuildExt4` runs `mkfs.ext4 -d <dir>` to populate an
   ext4 image from the directory with no mount and no root privileges. The size
   is derived from the extracted content with headroom and a floor.
5. Boot. The engine boots Firecracker on the built rootfs. Because the agent
   lives at `/init` and a normal (non-initramfs) root filesystem does not have
   `/init` in the kernel's default init search path, the engine appends
   `init=/init` to the boot args so the agent actually becomes PID 1.
6. Wait for readiness. The build connects to the guest agent over vsock and
   pings it. A successful ping is the boot-readiness signal: the agent only
   answers once it is up as PID 1, so this confirms the guest booted before
   anything is snapshotted. This wait ALWAYS runs, even with no init commands,
   so a half-booted VM is never captured.
7. Run init IN the VM. Each `spec.template.init` command runs inside the booted VM
   through the guest agent. If any command exits nonzero the build aborts and
   nothing is snapshotted (a template whose `pip install` failed must never be
   served). Init runs at BUILD TIME, before any claim-time env or secrets exist,
   by design.
8. Snapshot. The VM is paused and a full snapshot (`mem` + `vmstate`) is taken,
   its digest recorded in the CAS store, and the template marked verified.

A fork then restores that snapshot copy-on-write and the same agent answers in
each fork.

## The OCI-ref vs file-path heuristic

`spec.template.image` may be an OCI reference (`busybox:stable`, `python:3.12-slim`) or a
path to a pre-built rootfs file (back-compat for hand-built rootfs images and
tests). The engine decides as follows (`internal/fork/imageref.go`):

- If the string exists as a file on disk, it is a file path (copied as the
  rootfs, current behavior).
- If it begins with `/`, `./`, or `../`, it is treated as a path, never a ref.
- Otherwise, if it parses as an OCI reference it is built via the pipeline
  above.

This keeps the file-path path working for the existing hand-built rootfs while
making real OCI references build a rootfs.

## The agent binary requirement

Building from an image needs the guest agent binary to inject as `/init`. forkd
exposes it via `--agent-bin` (and an optional `--busybox-bin` static `/bin/sh`
source for shell-less images), plumbed through `fork.EngineOpts.AgentBinPath`
and `BusyboxPath`. For now forkd must be shipped or mounted with this binary
present. Building from an image with no agent binary configured fails loudly;
file-path templates do not need it.

## Init command semantics

- Init commands run INSIDE the booted template VM over the guest agent, not on
  the host.
- They run at build time, before claim-time secrets, so they are for baking the
  image (installing packages, warming caches), not for per-claim configuration.
- A nonzero exit aborts the build; the broken template is never snapshotted or
  served.
- `template.Spec.InitCommands()` is plumbed end to end: pool reconciler ->
  `CreateTemplateRequest.init_commands` -> forkd -> engine -> the VM. It returns
  the legacy `spec.template.init` list, or, when `spec.template.buildSteps` is set, the flattened
  run/env/workdir steps in order (see the code-first section below).

## CI proof

`cmd/tmpl-smoke` drives `fork.NewEngine` directly to build a template from
`busybox:stable` with an init command, fork it, and exec assertions over the
guest agent. The KVM CI job (`.github/workflows/kvm-test.yaml`) runs it and
gates on two assertions: the init command ran (it wrote `/built.txt`, readable
in the fork) and the image filesystem is present (`/bin/busybox` resolves).
Docker Hub pull flakes are retried and marked `PULL_FAILED` so a registry flake
is distinguished from a real pipeline failure; a registry mirror is the
production answer.

## Define a custom environment (code-first)

You do not have to hand-write the `SandboxPool` YAML. The Python SDK ships a
fluent `Template` builder that authors the spec from code, in the
shape E2B and Daytona use:

```python
from mitos import Template

spec = (
    Template()
    .from_image("python:3.12")
    .workdir("/app")
    .copy("app/", "/app")
    .env("PORT", "8080")
    .run("pip install -r requirements.txt")
    .set_start("python app.py")
    .cpu("2")
    .memory("1Gi")
    .to_spec()
)
```

`to_spec()` emits the `PoolTemplateSpec` dict; `to_pool("my-pool")` wraps
it in a full `SandboxPool` object you can apply to a cluster. The ordered step list maps onto
the CRD `spec.template.buildSteps` (copy / run / env / workdir); the build path flattens
run, env, and workdir steps into the in-VM init commands in order, so a template
authored with `buildSteps` builds exactly like one authored with `spec.template.init`. A
template may set either; `buildSteps` is the recommended code-first form.

### From the CLI, from a Dockerfile or a spec

```sh
# From a Dockerfile (Daytona create --dockerfile parity):
mitos template build --name web --dockerfile ./Dockerfile

# From a declarative spec file (YAML or JSON):
mitos template build --name web --spec ./template.yaml

# Publish a built template:
mitos template push web
```

`mitos template build` parses the source into a spec, prints the build plan
(which steps a cached build would reuse), and authors the `SandboxPool` with inline `spec.template`. The
node then builds the snapshot on a KVM host. A failing build step surfaces the
typed `build_failed` error (HTTP 422) whose `context` names the failing step
index and kind and whose remediation tells you to fix that step and rebuild.

### Fast cached builds

Each build step gets a content-addressed cache key chained over the base image
and every step before it (`internal/templatebuild`): the key at step N depends on
the base image and steps 0..N. Changing step N invalidates step N and every step
after it, but leaves the keys of steps 0..N-1 untouched, so a real build reuses
the unchanged prefix and rebuilds only from the first changed step. This is the
E2B-style fast-cached-build behavior. The key computation and the skip decision
are pure and unit-tested on any host; the actual layer reuse on a live boot is
KVM gated and asserted in the Firecracker suite.

## Guest process resource limits

The guest agent is PID 1 inside the microVM. A bare Linux init inherits a low
soft `RLIMIT_NOFILE` (commonly 1024 open files), and every process the agent
spawns for `exec`, PTY sessions, `run_code`, and serving workloads would inherit
that same low limit. Data libraries that open many files at import time (pandas,
openpyxl, scikit) hit `EMFILE` ("too many open files") under 1024, so a plain
`import pandas` could die inside a sandbox even when the host-side caps (husk pod
cgroup, per-sandbox stream caps) are nowhere near saturated. Those host caps do
not govern in-guest per-process rlimits; only `setrlimit` inside the guest does.

The agent therefore raises the soft `RLIMIT_NOFILE` of every process it spawns to
a sane default of 65536, clamped to the inherited hard limit and never lowered
below what the process already has. The raise is installed via a `pre_exec` hook
that runs in the forked child before `execve`, so it applies to the whole child
tree.

To override the default, set the `MITOS_RLIMIT_NOFILE` environment variable in
the guest agent's environment to a decimal file count of at least 64. It is read
once per spawn from the agent's own environment, so it applies uniformly to every
spawned process. A value below 64, or an unparseable value, is ignored with a
warning and the default applies; the floor stops a `0` or tiny typo from silently
crippling every spawned child. An operator may set a value below the default (but
at or above the floor) deliberately; the agent never raises the hard limit (that
needs `CAP_SYS_RESOURCE` and is out of scope).

The default and the override resolution are unit-tested on any host; the
end-to-end proof (a spawned guest process observing the raised limit and opening
more than 1024 files) is KVM gated and runs in the Firecracker suite.

## Open follow-ups

- A first-class `template.spec` field (for example `resources.rlimits.nofile`)
  plumbed through CreateTemplate and Fork into the guest agent's
  `MITOS_RLIMIT_NOFILE` boot environment, validated at the boundary, so operators
  set the limit declaratively instead of via the raw environment variable.
- `go:embed` the guest agent into the forkd binary so no external `--agent-bin`
  path is needed.
- OCI layer caching tied to the CAS store so repeated pool builds do not
  re-pull and re-extract.
- Registry credentials and private images, plus a pull-through mirror for
  reliability.
- Non-ext4 backends (erofs, virtio-fs).
