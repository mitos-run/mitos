# Red Hat Certified Operators path for Mitos

This runbook covers submitting Mitos to the
[Red Hat Certified Operators catalog](https://catalog.redhat.com) via
[redhat-openshift-ecosystem/certified-operators](https://github.com/redhat-openshift-ecosystem/certified-operators).

## Read this first: the fit risk

Mitos may not be a good fit for the certified (OpenShift) path, and we should
not claim OpenShift compatibility we have not verified. The certified catalog
exists to certify operators that run on OpenShift, and OpenShift is deliberately
locked down. Mitos pulls in the opposite direction:

- **KVM / nested virtualization.** Mitos boots and forks real Firecracker
  microVMs. The `forkd` DaemonSet needs `/dev/kvm` on every sandbox node. Many
  OpenShift clusters, especially managed ones, run on virtualized infrastructure
  without nested virtualization, so `/dev/kvm` is simply absent. OpenShift
  Virtualization (CNV) addresses VM workloads but is a different model than
  Mitos's host-level Firecracker forking and does not automatically grant Mitos
  what it needs.
- **Privileged DaemonSet.** `forkd` runs privileged, touches host paths, and
  needs device access. OpenShift gates this behind Security Context Constraints
  (SCCs). A privileged SCC binding is an explicit, audited cluster decision; the
  default `restricted-v2` SCC will reject the workload outright.
- **Bare-metal-style nodes.** The reference platform is bare metal (Hetzner plus
  Talos). OpenShift-on-bare-metal exists, but it is a narrower deployment shape
  than the certified catalog's typical audience.

Net: pursuing certification is only worthwhile once there is real demand for
Mitos on OpenShift-on-bare-metal with KVM and a privileged SCC available. Until
then, prioritize the community OperatorHub path (`docs/operatorhub.md`) and the
Helm chart. Do not advertise OpenShift support before it is verified on a real
OpenShift-on-bare-metal cluster with nested virt.

Treat everything below as the procedure to follow IF that demand materializes.

## 1. Red Hat Connect partner project

Certification runs through the
[Red Hat Partner Connect](https://connect.redhat.com) portal:

1. Create or join the partner company account.
2. Create a **Container application** project for each image that must be
   certified (at minimum the controller; in practice every image OLM or the
   controller pulls: `mitos-controller`, `mitos-forkd`, `mitos-husk-stub`,
   `mitos-kvm-device-plugin`, `mitos-facade`).
3. Create an **Operator bundle** certification project. This is the one tied to
   the certified-operators PR below.

Each project gets a project ID and a registry namespace under
`registry.connect.redhat.com`; certified images are ultimately served from
there, not from `ghcr.io`.

## 2. Certified image requirements

Certified container images have hard requirements the current `ghcr.io` images
likely do not meet yet:

- **UBI base image.** Each certified image must be built `FROM` a Red Hat
  Universal Base Image (`registry.access.redhat.com/ubi9/ubi-minimal` or
  similar). The Mitos Go binaries are statically linkable, so rebasing onto
  `ubi9-minimal` is feasible, but it is a real build change and must be done
  before submission.
- **Required labels.** Each image must carry:
  - `name`
  - `vendor`
  - `version`
  - `release`
  - `summary`
  - `description`
  - `maintainer`
  and ship a license file under `/licenses` (Apache-2.0 for Mitos).
- **No critical/important unresolved CVEs.** Red Hat scans the image; the UBI
  base keeps this tractable because Red Hat patches it.

## 3. Preflight checks (the human must run these)

Install the
[openshift-preflight](https://github.com/redhat-openshift-ecosystem/openshift-preflight)
tool. Preflight is not available in CI scratch environments here; the maintainer
runs it against a real OpenShift cluster they are logged into.

Per-image container check:

```bash
preflight check container registry.connect.redhat.com/mitos/mitos-controller:0.4.0 \
  --pyxis-api-token "$PYXIS_API_TOKEN" \
  --certification-project-id "$CONTROLLER_PROJECT_ID"
```

Operator bundle check (run against a live, KVM-capable OpenShift cluster with
the privileged SCC available, or it will fail on deployment):

```bash
preflight check operator "$BUNDLE_IMG" \
  --pyxis-api-token "$PYXIS_API_TOKEN" \
  --certification-project-id "$BUNDLE_PROJECT_ID"
```

Also run the operator-sdk validation with the Red Hat optional suites:

```bash
operator-sdk bundle validate ./deploy/olm/bundle \
  --select-optional suite=operatorframework

operator-sdk bundle validate ./deploy/olm/bundle \
  --select-optional name=community
```

Expect preflight to flag the SCC and KVM dependencies as deployment failures on
a stock OpenShift cluster. That is the fit risk made concrete; resolve it by
running on OpenShift-on-bare-metal with nested virt and a privileged SCC bound
to the `mitos-controller` and `forkd` service accounts, or do not pursue
certification.

## 4. The certified-operators PR

The target repo is
[redhat-openshift-ecosystem/certified-operators](https://github.com/redhat-openshift-ecosystem/certified-operators).
The directory layout mirrors community-operators:

```
operators/mitos/
  0.4.0/
    manifests/
      mitos.clusterserviceversion.yaml
      mitos.run_*.yaml
    metadata/
      annotations.yaml
  ci.yaml
```

Differences from the community submission:

- Image references in the CSV must point at
  `registry.connect.redhat.com/...` digests, not `ghcr.io` tags.
- The bundle must reference the certified, UBI-based, scanned images.
- The `ci.yaml` ties the PR to your bundle certification project ID:

  ```yaml
  cert_project_id: <bundle-project-id>
  ```

Open the PR; the Red Hat pipeline runs preflight and the certification suite and
gates merge on a clean OpenShift deployment.

## 5. Recommendation

Gate this entire path on verified OpenShift-on-bare-metal demand with KVM and a
privileged SCC. The honest default is: ship the Helm chart and the community
OperatorHub bundle, document the KVM and privileged-DaemonSet requirements
plainly, and revisit certification only when a customer needs it on OpenShift
and can provide the cluster shape to verify it on.
