# Air-gapped and offline install

mitos self-hosts fully offline. Nothing in the runtime dials a third-party SaaS:
billing, self-serve signup, and abuse checks are hosted-only surfaces that are
capability-gated off by default, so an air-gapped install is a clean, complete
product, not a degraded one. What an offline install needs is exactly three
things staged inside the boundary: the container images, the Helm chart, and the
per-node microVM assets (guest kernel and Firecracker). This runbook stages each
one and installs against them.

Do the staging from a connected mirror host, then move the artifacts across the
boundary by whatever mechanism your environment allows (internal registry,
tarballs on removable media).

## 1. Mirror the container images

Every mitos image lives under `ghcr.io/mitos-run`. Copy each to your internal
registry at the release tag you intend to run (`vX.Y.Z`). Use any registry
copier; `skopeo` and `crane` both work without a local Docker daemon.

```bash
SRC=ghcr.io/mitos-run
DST=registry.internal/mitos      # your internal registry
TAG=vX.Y.Z                       # the mitos release to run

for img in mitos-controller mitos-forkd mitos-husk-stub \
           mitos-kvm-device-plugin mitos-facade mitos-canary \
           mitos-gateway mitos-console; do
  skopeo copy "docker://$SRC/$img:$TAG" "docker://$DST/$img:$TAG"
done
```

The list above is the runtime set. `mitos-ci-runner` is a CI-only image and is
not needed to run mitos.

## 2. Mirror the Helm chart

The chart is published as an OCI artifact. Pull it on the connected host and
carry the `.tgz` across, or push it to your internal OCI registry.

```bash
# Pull to a local .tgz (carry this across the boundary):
helm pull oci://ghcr.io/mitos-run/charts/mitos --version X.Y.Z

# Or re-push to an internal OCI registry:
helm push mitos-X.Y.Z.tgz oci://registry.internal/mitos/charts
```

## 3. Stage the per-node microVM assets

KVM workers boot microVMs from a pinned guest kernel and a Firecracker binary.
These are node-level assets, provisioned outside the chart, so they must be
pre-staged on (or reachable from) each KVM worker:

- **Guest kernel**: the pinned `vmlinux` (see the version referenced by the
  build and bench workflows). Place it where the node's kernel provisioning
  expects it; see `host-prerequisites.md`.
- **Firecracker**: mitos runs a patched Firecracker (lazy-restore) built from
  `hack/install-firecracker-patched.sh`, which downloads a single release
  binary. Mirror that binary internally and run the script pointed at your
  mirror, or stage the binary onto each node directly. Stock Firecracker
  v1.15.0 also works, without the lazy-restore latency win.

Neither asset changes a snapshot's portable identity, so staging the same
kernel and Firecracker version across the fleet is all that is required.

## 4. Install against the mirror

Point the chart at your internal registry. `image.registry` is the single global
lever every component appends its repository to, and `global.imageTag` overrides
all component tags at once:

```bash
helm install mitos oci://registry.internal/mitos/charts/mitos \
  --version X.Y.Z -n mitos --create-namespace \
  --set image.registry=registry.internal/mitos \
  --set global.imageTag=vX.Y.Z
```

If the internal registry requires authentication, create a pull secret in the
install namespace and reference it via the chart's `imagePullSecrets` values;
`image.pullPolicy` defaults to `IfNotPresent`, which is correct offline.

## 5. Preflight and verify

`mitos doctor` is offline-safe: every check reads the local host, none dial out.
Run it on each KVM worker before and after install:

```bash
mitos doctor
```

Once the pods are up, the sandboxes themselves need no outbound internet: the
per-sandbox egress default is deny, so a workload only reaches the destinations
you allow. The platform requires no egress at all once the images, chart, and
microVM assets are local.

## What is deliberately absent offline

Per the self-host capability model, the hosted-only surfaces (self-serve signup,
billing and credits, Paddle, allowlist gating, abuse email checks) are gated off
and never appear: no dead links, no empty billing panels. The console, CLI, and
SDK point at a real first success against your own cluster.
