# Mitos Helm chart

Snapshot-fork sandboxes for AI agents on Kubernetes. This chart installs the
mitos.run operator: the controller Deployment, the forkd DaemonSet, the KVM
device plugin, the guest-kernel provisioner, the console and gateway front
door, and the five mitos.run CRDs. It is a faithful translation of the
kustomize manifests under `deploy/`.

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

The command above installs from a checkout. To install the published chart
instead, pull it straight from the OCI registry on GHCR (no `helm repo add`), the
same registry the component images live on. Pin `--version` to a release tag:

```
helm install mitos oci://ghcr.io/mitos-run/charts/mitos --version 1.25.0 -n mitos --set namespace.create=false
```

The published chart is indexed on Artifact Hub at
https://artifacthub.io/packages/helm/mitos/mitos.

The `crds/` directory installs the CRDs before the templated resources. Helm does
not upgrade or delete CRDs on chart upgrade or uninstall; apply CRD schema
changes yourself with `kubectl apply` against the manifests in `crds/`. The
exact procedure is in `docs/platforms/lifecycle.md` (CRD upgrades).

`namespace.create=true` renders a Namespace object with the PodSecurity labels
for the case where the chart is installed into a DIFFERENT namespace than the one
it creates; do not combine it with `--create-namespace` for the release namespace
(see above).

## Upgrading

Read the release notes, apply the CRD manifests for the target version, then
`helm upgrade` with the same values files you installed with. The full day-2
runbook (upgrade order, CRD step, rollback semantics, backup and restore of the
Postgres state, uninstall) is `docs/platforms/lifecycle.md`.

```
kubectl apply --server-side --force-conflicts -f deploy/charts/mitos/crds/
helm upgrade mitos mitos/mitos -n mitos --set namespace.create=false -f your-values.yaml
```

## Uninstall

```
helm uninstall mitos -n mitos
```

CRDs and any data they own are left in place by design. See
`docs/platforms/lifecycle.md` (Uninstalling) for the safe deletion order and
full cleanup.

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

Every per-component `image.tag` defaults to `""`, which resolves to the chart
`appVersion` (the release the chart shipped with); `global.imageTag` overrides
all of them at once. `deploy/charts/mitos/values.yaml` is the authoritative,
fully commented reference; the tables below cover the major knobs.

The chart ships a `values.schema.json`: every install, upgrade, template, and
lint validates your values against it, so an unknown or misspelled key (for
example `console.typoKey`) fails immediately instead of deploying silently
misconfigured. Free-form passthrough maps (`resources`, `commonLabels`,
ingress `annotations`, `extraEnv`, `nodeSelector`, `tolerations`,
`imagePullSecrets`, `controller.usage.priceList`, `console.billing.rates`)
still accept arbitrary keys by design.

### Images and core components

| Key | Default | Description |
| --- | --- | --- |
| `image.registry` | `ghcr.io/mitos-run` | Registry hosting every Mitos image. |
| `global.imageTag` | `""` | When set, overrides every per-component image tag. |
| `controller.image.repository` | `mitos-controller` | Controller image repository. |
| `controller.image.tag` | `""` | Controller image tag; empty uses the chart `appVersion`. |
| `controller.image.pullPolicy` | `IfNotPresent` | Controller image pull policy. |
| `controller.replicas` | `2` | Controller replica count (HA with leader election). |
| `controller.resources` | requests 128Mi/100m, limits 512Mi/500m | Controller resources. |
| `controller.enableHuskPods` | `true` | Render `--enable-husk-pods`. |
| `controller.huskDataDir` | `/var/lib/mitos` | Value for `--husk-data-dir`. |
| `controller.huskDnsUpstream` | `""` | When set, render `--husk-dns-upstream=<value>`; empty omits the flag. |
| `controller.kvmResourceName` | `mitos.run/kvm` | KVM extended-resource name husk pods request. |
| `controller.namespacedSecretsRBAC` | `false` | When true, remove the controller's cluster-wide Secrets grant; it instead binds itself to the `mitos-pool-secrets` ClusterRole per adopted pool namespace (+ a RoleBinding in its own namespace). Leave false until per-pool bindings have reconciled, then flip to narrow Secrets access. Multi-tenancy hardening. |
| `controller.orgTenancy.enabled` | `false` | Run the OrgReconciler: per-org isolation namespaces with quota, LimitRange, and default-deny NetworkPolicy. Off for single-tenant self-host. |
| `controller.usage.collector` | `false` | Run the per-org usage metering scraper and the bearer-gated internal usage API the console reads. |
| `controller.usage.tokenSecret.name` | `""` | Existing Secret holding the usage API bearer token (key `usage-api-token`). Empty renders no token and the API fails closed. |
| `controller.extraArgs` | `[]` | Extra args appended to the controller container. |
| `controller.extraEnv` | `[]` | Extra env vars appended to the controller container. |
| `huskStub.image.repository` | `mitos-husk-stub` | Husk stub image repository (passed via `--husk-stub-image`). |
| `huskStub.image.tag` | `""` | Husk stub image tag; empty uses the chart `appVersion`. |
| `forkd.image.repository` | `mitos-forkd` | forkd image repository. |
| `forkd.image.tag` | `""` | forkd image tag; empty uses the chart `appVersion`. |
| `forkd.resources` | requests 1Gi/500m, limits 16Gi/8 | forkd resources (includes the `mitos.run/kvm` device resource; do not remove it). |
| `forkd.dataDir` | `/var/lib/mitos` | forkd `--data-dir` and the data hostPath. |
| `forkd.enableNetworking` | `true` | Render `--enable-networking`. |
| `forkd.priorityClassName` | `system-node-critical` | Keeps forkd from kubelet eviction; set `""` to disable. |
| `forkd.seccompProfile` | `RuntimeDefault` | forkd seccomp profile; hardened runtimes (Talos) need a jailer-compatible profile, see `values/talos.yaml`. |
| `forkd.extraCapabilities` | `[]` | Capabilities ADDED to forkd's fixed builder set (hardened runtimes only). |
| `forkd.nodeSelector` | `mitos.run/kvm: "true"` | forkd and kernel-stage nodeSelector. |
| `forkd.tolerations` | `mitos.run/dedicated` NoSchedule | forkd and kernel-stage tolerations. |
| `devicePlugin.enabled` | `true` | Render the KVM device plugin DaemonSet. |
| `devicePlugin.image.repository` | `mitos-kvm-device-plugin` | Device plugin image repository. |
| `devicePlugin.image.tag` | `""` | Device plugin image tag; empty uses the chart `appVersion`. |
| `kernelProvisioner.enabled` | `true` | Render the kernel-stage DaemonSet. |
| `kernelProvisioner.kernelUrl` | Firecracker CI x86_64 5.10 vmlinux | Guest kernel download URL. |
| `kernelProvisioner.kernelSha256` | `""` | Expected SHA256 of the kernel. When set, the staged kernel is verified and the init container fails closed on mismatch. Strongly recommended; compute with `curl -fsSL <kernelUrl> \| sha256sum`. |
| `admissionWebhook.enabled` | `false` | Render the validating webhook that requires a Sandbox creator to be authorized to impersonate `spec.serviceAccount`. Strongly recommended in a multi-tenant cluster. Self-signed cert by default; prefer cert-manager for production rotation. |
| `admissionWebhook.failurePolicy` | `Fail` | Webhook failure policy. `Fail` rejects claims if the webhook is unreachable (fail closed); set `Ignore` only if availability outranks the principal guarantee. |
| `facade.enabled` | `false` | Render the agents.x-k8s.io facade (see `facade.*` in values.yaml for image, pool, and resources). |
| `monitoring.enabled` | `false` | Render the PrometheusRule, Grafana dashboard ConfigMap, and PodMonitors. |
| `monitoring.prometheusRuleRelease` | `prometheus` | `release` label the Prometheus Operator selects on. |
| `canary.enabled` | `false` | Render the synthetic fork/exec canary (needs a scoped API key; see `canary.*` in values.yaml). |

### Console, gateway, and database

| Key | Default | Description |
| --- | --- | --- |
| `console.enabled` | `true` | Render the web console (BFF + embedded SPA). |
| `console.edition` | `community` | `community` (self-host) or `hosted`; a runtime value, never a build fork. |
| `console.replicas` | `1` | Console replicas. OIDC session state is per-pod in memory, so more than 1 breaks login without a shared session store. |
| `console.signup` | `false` | Mount the public self-serve signup endpoints. |
| `console.oidc.issuerURL` | `""` | Browser-session OIDC issuer (Dex/Keycloak/Google/...). |
| `console.oidc.clientID` | `""` | OIDC client ID. |
| `console.oidc.clientSecretRef` | `""` | Existing Secret with key `client-secret`. |
| `console.oidc.redirectURL` | `""` | The registered redirect URI, normally `https://<console-host>/auth/callback`. Required when OIDC is configured. |
| `console.usage.url` | `""` | Usage API base URL; empty derives the in-cluster Service when `controller.usage.collector` is true. |
| `console.secrets.kube.enabled` | `true` | Kubernetes-native org secret provider. |
| `console.secrets.openbao.enabled` | `false` | OpenBao/Vault external secret provider (`address`, `perOrg`). |
| `console.billing.enabled` | `false` | Billing surface toggle. Off for community: the billing routes never mount. |
| `console.billing.paddle.apiKeySecretRef` | name `""`, key `api-key` | Existing Secret holding the Paddle API key; with `webhookSecretRef` also set, Paddle is the provider. |
| `console.billing.paddle.webhookSecretRef` | name `""`, key `webhook-secret` | Existing Secret holding the Paddle webhook signing secret. |
| `console.onboarding.verifyURL` | `""` | Base URL of the signup verification link; required when SMTP is configured. |
| `console.onboarding.clusterProvisioning` | `false` | A verified signup creates the Org CR (requires `controller.orgTenancy.enabled`). |
| `console.onboarding.smtp.host` | `""` | SMTP host for the verification email; empty uses the dev log sender. |
| `console.onboarding.smtp.credentialsSecretRef` | `""` | Existing Secret with keys `username` and `password`; never inlined. |
| `console.ingress.enabled` | `false` | Render the console Ingress (`className`, `host`, `annotations`). |
| `console.extraEnv` | `[]` | Extra env vars appended to the console container. |
| `gateway.enabled` | `true` | Render the public API gateway (API-key auth, org resolution, forwarding). |
| `gateway.replicas` | `2` | Gateway replica count. |
| `gateway.enforce.enabled` | `true` | Quotas, rate limits, size caps, and the abuse kill-switch. Disable only for a trusted single-tenant deployment; the bypass is logged. |
| `gateway.enforce.trustedProxyHops` | `0` | Trusted reverse-proxy hops for client-IP resolution; `0` does not trust `X-Forwarded-For`. |
| `gateway.singleTenantNamespace` | `""` | Pin all sandbox operations to one fixed namespace instead of per-org namespaces (QA/single-tenant). |
| `gateway.ingress.enabled` | `false` | Render the gateway Ingress (`className`, `host`, `annotations`). |
| `database.dsnSecretRef.name` | `""` | Existing Secret holding the Postgres DSN for the console and gateway (accounts, orgs, API keys, credit ledger). Empty falls back to in-memory storage: DEV ONLY, lost on restart. |
| `database.dsnSecretRef.key` | `dsn` | Key within that Secret holding the DSN. |

### Telemetry

| Key | Default | Description |
| --- | --- | --- |
| `telemetry.enabled` | `false` | Opt-in product telemetry. Off renders no telemetry env at all; see `docs/telemetry.md`. |
| `telemetry.optOut` | `false` | Force-disable even when enabled; `DO_NOT_TRACK=1` is honored independently. |
| `telemetry.endpoint` | `""` | Collector URL; required when enabled, empty fails closed. |
| `telemetry.saltSecretRef` | name `""`, key `salt` | Secret holding the org-id HMAC salt; without it the org id is dropped. |
| `telemetry.tokenSecretRef` | name `""`, key `token` | Optional collector bearer token Secret. |

### Namespace, pull secrets, labels

| Key | Default | Description |
| --- | --- | --- |
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

### Optional subsystems (one-line pointers)

Each block is off by default and fully documented inline in `values.yaml`:

| Key | Default | Description |
| --- | --- | --- |
| `expose.enabled` | `false` | The preview-proxy for authed wildcard sandbox URLs (`expose.domain`, TLS, OIDC tiers, the `mitos-expose` Secret). |
| `dex.enabled` | `false` | In-cluster Dex federating GitHub/Google into one OIDC issuer for the console. |
| `frontdoor.enabled` | `false` | Single-origin reverse proxy routing marketing and console paths. |
| `edge.enabled` | `false` | Cilium Gateway-API edge resources (Gateway, Certificate, HTTPRoutes). |
| `marketing.enabled` | `false` | Marketing static-site Deployment behind the frontdoor. |

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
