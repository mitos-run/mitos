# mitos Helm chart

Snapshot-fork sandboxes for AI agents on Kubernetes. This chart installs the
mitos.run operator: the controller Deployment, the privileged forkd DaemonSet,
the KVM device plugin, the guest-kernel provisioner, and the six mitos.run CRDs.
It is a faithful translation of the kustomize manifests under `deploy/`.

## Prerequisites

- Kubernetes >= 1.29.
- Helm v3.
- At least one KVM-capable worker node labeled `mitos.run/kvm=true`. forkd and
  the husk pods only schedule on these nodes; the forkd and kernel-stage
  DaemonSets stay Pending until a matching node joins. PodSecurity admission must
  permit privileged pods in the install namespace; the chart sets
  `pod-security.kubernetes.io/enforce: privileged` on the namespace it creates.
- A registry pull credential for the mitos-* images (see Image pull secret below).

Label a KVM node:

```
kubectl label node <node-name> mitos.run/kvm=true
```

## Install

forkd, the husk pods, and the device plugin are privileged and the install
namespace MUST carry `pod-security.kubernetes.io/enforce: privileged`. Helm
cannot both create its release namespace AND apply those labels (it needs the
namespace to exist before it can store the release), and `--create-namespace`
makes an UNLABELED namespace that PSA `restricted` then rejects. So create the
labeled namespace first, then install with `namespace.create=false`:

```
kubectl create namespace mitos
kubectl label namespace mitos \
  pod-security.kubernetes.io/enforce=privileged \
  pod-security.kubernetes.io/warn=privileged \
  pod-security.kubernetes.io/audit=privileged
helm install mitos deploy/charts/mitos -n mitos --set namespace.create=false
```

The `crds/` directory installs the CRDs before the templated resources. Helm does
not upgrade or delete CRDs on chart upgrade or uninstall; manage CRD schema
changes out of band.

`namespace.create=true` renders a Namespace object with the PodSecurity labels
for the case where the chart is installed into a DIFFERENT namespace than the one
it creates; do not combine it with `--create-namespace` for the release namespace
(see above).

## Uninstall

```
helm uninstall mitos -n mitos
```

CRDs and any data they own are left in place by design.

## Image pull secret

The mitos-* images live in a private registry. By default the chart does NOT
render a pull secret (`imagePullSecret.create=false`); create one out of band:

```
kubectl create secret docker-registry ghcr-pull \
  --namespace mitos \
  --docker-server=ghcr.io \
  --docker-username=<github-username> \
  --docker-password=<ghcr-read-packages-PAT>
```

Or set `imagePullSecret.create=true` and pass a real
`imagePullSecret.dockerconfigjson`. Both the controller ServiceAccount and the
forkd DaemonSet reference the secret named by `imagePullSecret.name`.

## Values

| Key | Default | Description |
| --- | --- | --- |
| `image.registry` | `ghcr.io/paperclipinc` | Registry hosting every mitos image. |
| `global.imageTag` | `""` | When set, overrides every per-component image tag. |
| `controller.image.repository` | `mitos-controller` | Controller image repository. |
| `controller.image.tag` | `v0.4.0` | Controller image tag. |
| `controller.image.pullPolicy` | `IfNotPresent` | Controller image pull policy. |
| `controller.replicas` | `2` | Controller replica count. |
| `controller.resources` | requests 128Mi/100m, limits 512Mi/500m | Controller resources. |
| `controller.enableHuskPods` | `true` | Render `--enable-husk-pods`. |
| `controller.huskDataDir` | `/var/lib/mitos` | Value for `--husk-data-dir`. |
| `controller.huskDnsUpstream` | `""` | When set, render `--husk-dns-upstream=<value>`; empty omits the flag. |
| `controller.kvmResourceName` | `mitos.run/kvm` | KVM extended-resource name husk pods request. |
| `controller.extraArgs` | `[]` | Extra args appended to the controller container. |
| `huskStub.image.repository` | `mitos-husk-stub` | Husk stub image repository (passed via `--husk-stub-image`). |
| `huskStub.image.tag` | `v0.4.0` | Husk stub image tag. |
| `huskStub.image.pullPolicy` | `IfNotPresent` | Husk stub image pull policy. |
| `forkd.image.repository` | `mitos-forkd` | forkd image repository. |
| `forkd.image.tag` | `v0.4.0` | forkd image tag. |
| `forkd.image.pullPolicy` | `IfNotPresent` | forkd image pull policy. |
| `forkd.resources` | requests 1Gi/500m, limits 16Gi/8 | forkd resources. |
| `forkd.dataDir` | `/var/lib/mitos` | forkd `--data-dir` and the data hostPath. |
| `controller.namespacedSecretsRBAC` | `false` | When true, remove the controller's cluster-wide Secrets grant; it instead binds itself to the `mitos-pool-secrets` ClusterRole per adopted pool namespace (+ a RoleBinding in its own namespace). Leave false until per-pool bindings have reconciled, then flip to narrow Secrets access. Multi-tenancy hardening. |
| `forkd.enableNetworking` | `true` | Render `--enable-networking`. |
| `forkd.nodeSelector` | `mitos.run/kvm: "true"` | forkd and kernel-stage nodeSelector. |
| `forkd.tolerations` | `mitos.run/dedicated` NoSchedule | forkd and kernel-stage tolerations. |
| `devicePlugin.enabled` | `true` | Render the KVM device plugin DaemonSet. |
| `devicePlugin.image.repository` | `mitos-kvm-device-plugin` | Device plugin image repository. |
| `devicePlugin.image.tag` | `v0.4.0` | Device plugin image tag. |
| `devicePlugin.image.pullPolicy` | `IfNotPresent` | Device plugin image pull policy. |
| `kernelProvisioner.enabled` | `true` | Render the kernel-stage DaemonSet. |
| `kernelProvisioner.kernelUrl` | Firecracker CI x86_64 5.10 vmlinux | Guest kernel download URL. |
| `kernelProvisioner.kernelSha256` | `""` | Expected SHA256 of the kernel. When set, the staged kernel is verified and the init container fails closed on mismatch. Strongly recommended; compute with `curl -fsSL <kernelUrl> \| sha256sum`. |
| `admissionWebhook.enabled` | `false` | Render the validating webhook that requires a SandboxClaim creator to be authorized to impersonate `spec.serviceAccount`. Strongly recommended with `controller.workspaceMemorySnapshots` in a multi-tenant cluster. Self-signed cert by default; prefer cert-manager for production rotation. |
| `admissionWebhook.failurePolicy` | `Fail` | Webhook failure policy. `Fail` rejects claims if the webhook is unreachable (fail closed); set `Ignore` only if availability outranks the principal guarantee. |
| `facade.enabled` | `false` | Render the agents.x-k8s.io facade. |
| `facade.image.repository` | `mitos-facade` | Facade image repository. |
| `facade.image.tag` | `v0.4.0` | Facade image tag. |
| `facade.image.pullPolicy` | `IfNotPresent` | Facade image pull policy. |
| `facade.defaultPool` | `default` | Facade `--default-pool`. |
| `facade.clusterDomain` | `cluster.local` | Facade `--cluster-domain`. |
| `facade.resources` | requests 128Mi/100m, limits 256Mi/500m | Facade resources. |
| `monitoring.enabled` | `false` | Render the PrometheusRule and Grafana dashboard ConfigMap. |
| `monitoring.prometheusRuleRelease` | `prometheus` | `release` label the Prometheus Operator selects on. |
| `namespace.name` | `mitos` | Install namespace. |
| `namespace.create` | `true` | Render the Namespace with PodSecurity labels. |
| `imagePullSecret.create` | `false` | Render the pull-secret Secret. |
| `imagePullSecret.name` | `ghcr-pull` | Pull-secret name referenced by the workloads. |
| `imagePullSecret.dockerconfigjson` | `{"auths":{}}` base64 | Base64 dockerconfigjson value. |
| `imagePullSecrets` | `[{name: ghcr-pull}]` | Pull-secret names attached to every workload pod. |
| `commonLabels` | `{}` | Extra labels merged onto every resource. |

## Security notes

The chart reproduces the security-critical fields of the source manifests
verbatim and does not expose them as knobs:

- forkd runs `privileged: true` with hostPath mounts for `/var/lib/mitos` and the
  `/dev/kvm` char device. It is the privileged snapshot builder; the jailer-in-pod
  follow-up will narrow this. See `docs/threat-model.md`.
- The controller, facade, and device plugin run unprivileged with
  `allowPrivilegeEscalation: false`, `readOnlyRootFilesystem: true`, all
  capabilities dropped, and the controller and facade pods set
  `runAsNonRoot: true` with the RuntimeDefault seccomp profile.
- The device plugin mounts the host `/dev` read-only at `/host-dev` and the
  kubelet device-plugins dir; it advertises `mitos.run/kvm` only where `/dev/kvm`
  exists.
