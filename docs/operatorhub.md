# Submitting Mitos to OperatorHub.io

This runbook covers packaging the Mitos OLM bundle and submitting it to
[operatorhub.io](https://operatorhub.io) via the
[k8s-operatorhub/community-operators](https://github.com/k8s-operatorhub/community-operators)
repository.

The bundle skeleton lives at `deploy/olm/bundle/`:

```
deploy/olm/bundle/
  bundle.Dockerfile
  manifests/
    mitos.clusterserviceversion.yaml
    mitos.run_sandboxpools.yaml
    mitos.run_sandboxes.yaml
    mitos.run_workspaces.yaml
    mitos.run_workspacerevisions.yaml
  metadata/
    annotations.yaml
```

## 0. Prerequisite: images must exist

The CSV deploys `ghcr.io/mitos-run/mitos-controller:v0.4.0` and references
`ghcr.io/mitos-run/mitos-husk-stub:v0.4.0`. The controller in turn deploys the
node images (`mitos-forkd`, `mitos-kvm-device-plugin`, `mitos-facade`). The
project is migrating its registry from `ghcr.io/paperclipinc` to
`ghcr.io/mitos-run`. Before this bundle can install and run, every referenced
image MUST be published and pullable under `ghcr.io/mitos-run`. Confirm this
first; OLM will report ImagePullBackOff otherwise.

Mitos also requires KVM nodes and a privileged DaemonSet (see the README and
`docs/redhat-certification.md`). Community OperatorHub does not test against
your hardware, but reviewers will read the CSV requirements section. Keep it
honest.

## 1. Install the tooling

You need `operator-sdk` and `opm`. The maintainer running this MUST install and
run these locally; they are not available in CI scratch environments and the
validation step below cannot be run for you.

```bash
# operator-sdk (pick a recent release; v1.34.1 or newer)
export ARCH=$(uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/')
export OS=$(uname | tr '[:upper:]' '[:lower:]')
curl -LO "https://github.com/operator-framework/operator-sdk/releases/download/v1.34.1/operator-sdk_${OS}_${ARCH}"
chmod +x operator-sdk_${OS}_${ARCH} && sudo mv operator-sdk_${OS}_${ARCH} /usr/local/bin/operator-sdk

# opm
curl -LO "https://github.com/operator-framework/operator-registry/releases/download/v1.43.1/${OS}-${ARCH}-opm"
chmod +x ${OS}-${ARCH}-opm && sudo mv ${OS}-${ARCH}-opm /usr/local/bin/opm

operator-sdk version
opm version
```

## 2. Validate the bundle (the human must run this)

This is the command to run from the repo root. It is the exact validation the
community-operators CI will also run, so fix anything it flags before opening a
PR:

```bash
operator-sdk bundle validate ./deploy/olm/bundle --select-optional suite=operatorframework
```

Also run the default validators:

```bash
operator-sdk bundle validate ./deploy/olm/bundle
```

Common things this catches and how to fix them:

- Missing or malformed `alm-examples`: the annotation must be valid JSON. It is
  generated from `examples/`; re-derive it if you change those files.
- CSV `spec.version` must match the directory version `0.4.0` and the
  `metadata.name` suffix (`mitos.v0.4.0`).
- Every owned CRD in the CSV must have a matching CRD manifest in `manifests/`,
  and the `version` in each `customresourcedefinitions.owned` entry must be the
  CRD storage version. NOTE: every CRD is served and stored at `mitos.run/v1`.
  The CSV already reflects this. If you bump a CRD version, update both the CRD
  YAML and the CSV owned entry.
- Image references must be digest-pinnable; community-operators may require
  immutable references at submission time. Replace the `:v0.4.0` tags with
  `@sha256:...` digests if the reviewer asks.

## 3. Build and push the bundle image

```bash
export BUNDLE_IMG=ghcr.io/mitos-run/mitos-bundle:v0.4.0
docker build -f deploy/olm/bundle/bundle.Dockerfile -t "$BUNDLE_IMG" deploy/olm/bundle
docker push "$BUNDLE_IMG"
```

## 4. Test the bundle on a real cluster

On a KVM-capable cluster with OLM installed (`operator-sdk olm install` if it is
not), run the bundle directly:

```bash
kubectl create namespace mitos
operator-sdk run bundle "$BUNDLE_IMG" --namespace mitos
```

Watch the install:

```bash
kubectl get csv -n mitos -w
kubectl get deploy,pods -n mitos
```

Then apply an example CR and confirm the controller reconciles it:

```bash
kubectl apply -n mitos -f examples/python-pool.yaml
kubectl get sandboxpools,sandboxes -n mitos
```

Clean up:

```bash
operator-sdk cleanup mitos --namespace mitos
```

If the controller pod is Pending or ImagePullBackOff, recheck step 0 (images)
and that the cluster nodes expose `/dev/kvm`.

## 5. Open the community-operators PR

The target repo is
[k8s-operatorhub/community-operators](https://github.com/k8s-operatorhub/community-operators).
Fork it, then add the bundle under the versioned directory:

```
operators/mitos/
  0.4.0/
    manifests/
      mitos.clusterserviceversion.yaml
      mitos.run_sandboxpools.yaml
      mitos.run_sandboxes.yaml
      mitos.run_workspaces.yaml
      mitos.run_workspacerevisions.yaml
    metadata/
      annotations.yaml
  ci.yaml          # optional: reviewers, update graph policy
```

Copy `deploy/olm/bundle/manifests` and `deploy/olm/bundle/metadata` straight
into `operators/mitos/0.4.0/`. Do not copy `bundle.Dockerfile`; the
community-operators pipeline builds its own.

Suggested `operators/mitos/ci.yaml`:

```yaml
updateGraph: replaces-mode
reviewers:
  - <your-github-handle>
```

Then:

```bash
git checkout -b operator-mitos-0.4.0
git add operators/mitos/0.4.0
git commit -s -m "operator mitos (0.4.0)"
git push origin operator-mitos-0.4.0
```

Open the PR against `main`. The pipeline reruns
`operator-sdk bundle validate` plus deployment tests on a Kubernetes runner.
Address every failure it reports; the maintainer (not CI here) owns getting
validation green. Sign-off (`-s`) and the DCO check are required.

## Notes

- Channel: `alpha` only, default `alpha`. Mitos is pre-1.0 and the CSV maturity
  is `alpha`.
- For the next version, add `operators/mitos/0.5.0/` and set the CSV
  `spec.replaces: mitos.v0.4.0` (or `spec.skips`) so the update graph is
  well-formed.
- The Red Hat certified catalog is a separate submission with stricter
  requirements; see `docs/redhat-certification.md`.
