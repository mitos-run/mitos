# Mitos Helm chart

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

The published mitos-* images are public on GHCR, so no registry credential is
needed for a default install. A pull secret is only required for a private mirror
(see Image pull secret below).

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

The command above installs from a checkout. To install the published chart from
the mitos.run Helm repository instead:

```
helm repo add mitos https://mitos.run/charts
helm repo update
helm install mitos mitos/mitos -n mitos --set namespace.create=false
```

Or install straight from the OCI registry on GHCR (no `helm repo add`), the same
registry the component images live on:

```
helm install mitos oci://ghcr.io/mitos-run/charts/mitos -n mitos --set namespace.create=false
```

The published chart is indexed on Artifact Hub at
https://artifacthub.io/packages/helm/mitos/mitos.

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

The published mitos-* images on `ghcr.io/mitos-run` are public, so the default
install pulls them with no credential and attaches no pull secret
(`imagePullSecrets: []`). A release guard (`verify-public-images` in
`.github/workflows/publish.yaml`) fails the release if any chart-referenced image
is private or missing, so this stays true across releases.

You only need a pull secret for a private mirror or a private re-publish of the
images. In that case point `image.registry` at your mirror, then create the
secret and reference it:

```
kubectl create secret docker-registry ghcr-pull \
  --namespace mitos \
  --docker-server=<your-registry> \
  --docker-username=<username> \
  --docker-password=<read-packages-token>
helm install mitos deploy/charts/mitos -n mitos --set namespace.create=false \
  --set 'imagePullSecrets[0].name=ghcr-pull'
```

Or let the chart render the secret: set `imagePullSecret.create=true`, pass a
real base64 `imagePullSecret.dockerconfigjson`, and add `imagePullSecret.name` to
`imagePullSecrets`.

If a pod reports `ImagePullBackOff`, you are almost certainly on a private mirror
without one of the above. The forkd DaemonSet's registry-credential mount is
already optional, so a missing secret never blocks forkd from starting.

## Values

| Key | Default | Description |
| --- | --- | --- |
| `image.registry` | `ghcr.io/mitos-run` | Registry hosting every Mitos image. |
| `global.imageTag` | `""` | When set, overrides every per-component image tag. |
| `controller.image.repository` | `mitos-controller` | Controller image repository. |
| `controller.image.tag` | `v0.13.0` | Controller image tag. |
| `controller.image.pullPolicy` | `IfNotPresent` | Controller image pull policy. |
| `controller.replicas` | `2` | Controller replica count. |
| `controller.resources` | requests 128Mi/100m, limits 512Mi/500m | Controller resources. |
| `controller.enableHuskPods` | `true` | Render `--enable-husk-pods`. |
| `controller.huskDataDir` | `/var/lib/mitos` | Value for `--husk-data-dir`. |
| `controller.huskDnsUpstream` | `""` | When set, render `--husk-dns-upstream=<value>`; empty omits the flag. |
| `controller.kvmResourceName` | `mitos.run/kvm` | KVM extended-resource name husk pods request. |
| `controller.extraArgs` | `[]` | Extra args appended to the controller container. |
| `huskStub.image.repository` | `mitos-husk-stub` | Husk stub image repository (passed via `--husk-stub-image`). |
| `huskStub.image.tag` | `v0.13.0` | Husk stub image tag. |
| `huskStub.image.pullPolicy` | `IfNotPresent` | Husk stub image pull policy. |
| `forkd.image.repository` | `mitos-forkd` | forkd image repository. |
| `forkd.image.tag` | `v0.13.0` | forkd image tag. |
| `forkd.image.pullPolicy` | `IfNotPresent` | forkd image pull policy. |
| `forkd.resources` | requests 1Gi/500m, limits 16Gi/8 | forkd resources. |
| `forkd.dataDir` | `/var/lib/mitos` | forkd `--data-dir` and the data hostPath. |
| `controller.namespacedSecretsRBAC` | `false` | When true, remove the controller's cluster-wide Secrets grant; it instead binds itself to the `mitos-pool-secrets` ClusterRole per adopted pool namespace (+ a RoleBinding in its own namespace). Leave false until per-pool bindings have reconciled, then flip to narrow Secrets access. Multi-tenancy hardening. |
| `forkd.enableNetworking` | `true` | Render `--enable-networking`. |
| `forkd.nodeSelector` | `mitos.run/kvm: "true"` | forkd and kernel-stage nodeSelector. |
| `forkd.tolerations` | `mitos.run/dedicated` NoSchedule | forkd and kernel-stage tolerations. |
| `devicePlugin.enabled` | `true` | Render the KVM device plugin DaemonSet. |
| `devicePlugin.image.repository` | `mitos-kvm-device-plugin` | Device plugin image repository. |
| `devicePlugin.image.tag` | `v0.13.0` | Device plugin image tag. |
| `devicePlugin.image.pullPolicy` | `IfNotPresent` | Device plugin image pull policy. |
| `kernelProvisioner.enabled` | `true` | Render the kernel-stage DaemonSet. |
| `kernelProvisioner.kernelUrl` | Firecracker CI x86_64 5.10 vmlinux | Guest kernel download URL. |
| `kernelProvisioner.kernelSha256` | `""` | Expected SHA256 of the kernel. When set, the staged kernel is verified and the init container fails closed on mismatch. Strongly recommended; compute with `curl -fsSL <kernelUrl> \| sha256sum`. |
| `admissionWebhook.enabled` | `false` | Render the validating webhook that requires a Sandbox creator to be authorized to impersonate `spec.serviceAccount`. Strongly recommended with `controller.workspaceMemorySnapshots` in a multi-tenant cluster. Self-signed cert by default; prefer cert-manager for production rotation. |
| `admissionWebhook.failurePolicy` | `Fail` | Webhook failure policy. `Fail` rejects claims if the webhook is unreachable (fail closed); set `Ignore` only if availability outranks the principal guarantee. |
| `facade.enabled` | `false` | Render the agents.x-k8s.io facade. |
| `facade.image.repository` | `mitos-facade` | Facade image repository. |
| `facade.image.tag` | `v0.13.0` | Facade image tag. |
| `facade.image.pullPolicy` | `IfNotPresent` | Facade image pull policy. |
| `facade.defaultPool` | `default` | Facade `--default-pool`. |
| `facade.clusterDomain` | `cluster.local` | Facade `--cluster-domain`. |
| `facade.resources` | requests 128Mi/100m, limits 256Mi/500m | Facade resources. |
| `monitoring.enabled` | `false` | Render the PrometheusRule and Grafana dashboard ConfigMap. |
| `monitoring.prometheusRuleRelease` | `prometheus` | `release` label the Prometheus Operator selects on. |
| `namespace.name` | `mitos` | Install namespace. |
| `namespace.create` | `true` | Render the Namespace with PodSecurity labels. |
| `imagePullSecret.create` | `false` | Render the dockerconfigjson Secret named below. Public images need none; set true only for a private mirror. |
| `imagePullSecret.name` | `ghcr-pull` | Name of the Secret rendered when `create=true`. |
| `imagePullSecret.dockerconfigjson` | `{"auths":{}}` base64 | Base64 dockerconfigjson value used when `create=true`. |
| `imagePullSecrets` | `[]` | Pull-secret names attached to every workload pod. Empty by default (public images); populate for a private mirror. |
| `commonLabels` | `{}` | Extra labels merged onto every resource. |
| `controller.usage.priceList` | `{}` | Display price list for the internal usage API (`MITOS_USAGE_PRICELIST`, dollars per unit, rendered as one JSON env). Non-empty REPLACES the illustrative defaults; unknown keys or negative values fail controller startup. Keep consistent with `console.billing.rates`. |
| `console.signupCreditCents` | `0` | Signup credit for a newly verified org in integer cents (`MITOS_CONSOLE_SIGNUP_CREDIT_CENTS`). 0 renders no env (binary default applies). |
| `console.autoAllowDomains` | `[]` | Email domains whose signups bypass manual allowlist approval (`MITOS_CONSOLE_AUTOALLOW_DOMAINS`, comma-joined). Empty renders no env; the binary defaults to `mitos.run`. |
| `console.authConnectors` | `[]` | Social-login connectors advertised at `GET /auth/connectors` (`MITOS_CONSOLE_AUTH_CONNECTORS`). Known values: `github`, `google`. |
| `console.antiAbuse.friendlyCaptcha.siteKey` | `""` | Friendly Captcha sitekey (`MITOS_CONSOLE_FRIENDLY_CAPTCHA_SITEKEY`). Captcha env renders only when both the sitekey and the secret ref are set. |
| `console.antiAbuse.friendlyCaptcha.secretRef` | `name: ""`, `key: friendly-captcha-secret` | Existing Secret holding the Friendly Captcha API secret, injected via secretKeyRef ONLY (`MITOS_CONSOLE_FRIENDLY_CAPTCHA_SECRET`). |
| `console.antiAbuse.friendlyCaptcha.url` | `""` | Verification API base URL override (`MITOS_CONSOLE_FRIENDLY_CAPTCHA_URL`). |
| `console.antiAbuse.disposableAllowDomains` | `[]` | Email domains exempted from the disposable-domain blocklist (`MITOS_CONSOLE_DISPOSABLE_ALLOW`, comma-joined). |
| `console.antiAbuse.signupIPLimit` | `""` | Per-IP signup velocity cap (`MITOS_CONSOLE_SIGNUP_IP_LIMIT`). Empty renders no env (binary default 10 per 1h); `"0"` disables the cap. |
| `console.antiAbuse.signupIPWindow` | `""` | Velocity window as a Go duration (`MITOS_CONSOLE_SIGNUP_IP_WINDOW`), e.g. `30m`. |
| `console.billing.paddle.topUpProduct` | `""` | Paddle product id for prepaid credit top-up checkout (`MITOS_CONSOLE_PADDLE_TOPUP_PRODUCT`). Empty disables the affordance. |
| `console.billing.paddle.currency` | `""` | ISO currency code for top-up checkout (`MITOS_CONSOLE_PADDLE_CURRENCY`). Empty uses the binary default (EUR). |
| `console.billing.rates` | `{}` | Billing rate table (`MITOS_CONSOLE_RATES`, milli-cents per unit, rendered as one JSON env). Non-empty REPLACES the illustrative defaults entirely; unknown keys or negative values fail console startup. Use a values file or `--set-json` for fractional values. See `docs/saas/pricing.md`. |

## Security notes

The chart reproduces the security-critical fields of the source manifests
verbatim and does not expose them as knobs:

- forkd runs `privileged: true` with hostPath mounts for `/var/lib/mitos` and the
  `/dev/kvm` char device. It is the privileged snapshot builder. See
  `docs/threat-model.md`.
- The controller, facade, and device plugin run unprivileged with
  `allowPrivilegeEscalation: false`, `readOnlyRootFilesystem: true`, all
  capabilities dropped, and the controller and facade pods set
  `runAsNonRoot: true` with the RuntimeDefault seccomp profile.
- The device plugin mounts the host `/dev` read-only at `/host-dev` and the
  kubelet device-plugins dir; it advertises `mitos.run/kvm` only where `/dev/kvm`
  exists.
