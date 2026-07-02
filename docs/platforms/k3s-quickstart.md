# k3s single-user quickstart

This is the fastest way to get a working Mitos install with a console you can log
into, on a single [k3s](https://k3s.io) node, gated behind ONE static
username/password. It is meant for first-run evaluation and solo QA. k3s is much
easier to stand up than Talos for a first try; for a production bare-metal fleet,
see `talos-hetzner.md`.

> WARNING: single-user QA / first-run ONLY. NOT FOR PRODUCTION. NOT MULTI-TENANT.
>
> The login gate in this guide (`examples/single-user-dex`) is one static
> credential over plaintext-HTTP Dex with a self-signed console certificate. It
> does NOT provide tenant isolation: Mitos sandboxes are microVMs, not pods, and
> nothing here governs sandbox isolation. For more than one user or any
> production use, run a real identity provider (the chart's federated Dex, or
> Keycloak / Okta / Google) and a trusted TLS certificate.

## Verification status

Honesty rule: a step is only marked verified when it was actually run. No cluster
was available when this guide was written, so the end-to-end cluster steps are
marked accordingly.

| Item | Status | Notes |
|------|--------|-------|
| `examples/single-user-dex` manifests parse as valid YAML | VALIDATED | parsed locally |
| `kubectl kustomize examples/single-user-dex` renders | VALIDATED | rendered locally |
| OIDC value/env names match what `cmd/console` reads | VERIFIED | traced against `cmd/console/main.go` and the chart |
| k3s install + Helm install on a live node | NOT VERIFIED HERE | requires a host; steps are the intended path |
| Console login with the static user end to end | NOT VERIFIED HERE | requires a cluster |
| Firecracker fork on `/dev/kvm` | HARDWARE-REQUIRED | needs hardware virtualization on the node |

## What you need

- A Linux host you control. To actually fork microVMs, the host must expose
  hardware virtualization (`/dev/kvm`): bare metal, or a VM with nested
  virtualization enabled. Confirm with `egrep -c 'vmx|svm' /proc/cpuinfo` (a
  nonzero count) and `test -e /dev/kvm`. Without KVM the control plane installs
  and the console works, but forkd cannot create real sandboxes.
- `kubectl`, `helm`, `htpasswd` (from `apache2-utils` / `httpd-tools`), and
  `openssl` on your workstation.

## 1. Install k3s

```bash
curl -sfL https://get.k3s.io | sh -
```

This installs a single-node k3s with Traefik as the Ingress controller, which the
single-user-dex example relies on. Point `kubectl` at the cluster:

```bash
export KUBECONFIG=/etc/rancher/k3s/k3s.yaml
kubectl get nodes
```

(Copy the kubeconfig to your workstation if you are driving the cluster
remotely; replace `127.0.0.1` in the server URL with the node's reachable IP.)

## 2. Label the node for KVM

forkd schedules onto nodes labelled for KVM. Even for evaluation, set the label
so the daemon and husk pods land:

```bash
kubectl label node "$(kubectl get nodes -o jsonpath='{.items[0].metadata.name}')" mitos.run/kvm=true
```

## 3. Create the install namespace

forkd, the husk pods, and the device plugin are privileged, so the namespace MUST
carry the PodSecurity `privileged` labels. `helm --create-namespace` would make an
unlabeled namespace that the `restricted` policy then rejects, so create and label
it first:

```bash
kubectl create namespace mitos
kubectl label namespace mitos \
  pod-security.kubernetes.io/enforce=privileged \
  pod-security.kubernetes.io/warn=privileged \
  pod-security.kubernetes.io/audit=privileged
```

## 4. Install the Mitos chart

From a checkout:

```bash
helm install mitos deploy/charts/mitos -n mitos --set namespace.create=false
```

Or from the published OCI chart (no checkout needed):

```bash
helm install mitos oci://ghcr.io/mitos-run/charts/mitos -n mitos --set namespace.create=false
```

See `deploy/charts/mitos/README.md` for the full value reference and the image
pull-secret notes. Wait for the controller and console to become Ready:

```bash
kubectl get pods -n mitos -w
```

## 5. Gate the console behind one user

Follow `examples/single-user-dex/README.md`. In short:

1. Derive hostnames from the node IP (both must resolve from the browser and from
   inside the cluster; `nip.io` does this on a single node):

   ```bash
   export NODE_IP=$(kubectl get nodes -o jsonpath='{.items[0].status.addresses[?(@.type=="InternalIP")].address}')
   export DEX_HOST="dex.${NODE_IP}.nip.io"
   export CONSOLE_HOST="console.${NODE_IP}.nip.io"
   ```

2. Stamp the hostnames into copies of the example files:

   ```bash
   cd examples/single-user-dex
   for f in dex.yaml console-ingress.yaml console-oidc-values.yaml; do
     sed -e "s/dex.example.com/${DEX_HOST}/g" \
         -e "s/console.example.com/${CONSOLE_HOST}/g" \
         "$f" > "/tmp/${f}"
   done
   ```

3. Set your password: generate a bcrypt hash and paste it (and your email) into
   `/tmp/dex.yaml` (`staticPasswords[0]`). The committed hash is an unusable
   placeholder.

   ```bash
   htpasswd -bnBC 10 "" 'your-strong-password' | tr -d ':\n' | sed 's/^\$2y\$/\$2a\$/'
   ```

4. Create the shared client-secret Secret and the self-signed console TLS Secret:

   ```bash
   kubectl create secret generic mitos-console-oidc -n mitos \
     --from-literal=client-secret="$(openssl rand -hex 32)"

   openssl req -x509 -newkey rsa:2048 -nodes -days 365 \
     -keyout /tmp/console.key -out /tmp/console.crt \
     -subj "/CN=${CONSOLE_HOST}" -addext "subjectAltName=DNS:${CONSOLE_HOST}"
   kubectl create secret tls mitos-console-tls -n mitos \
     --cert=/tmp/console.crt --key=/tmp/console.key
   ```

5. Apply Dex and the console Ingress, then wire the console:

   ```bash
   kubectl apply -n mitos -f /tmp/dex.yaml -f /tmp/console-ingress.yaml
   helm upgrade mitos deploy/charts/mitos -n mitos \
     --set namespace.create=false -f /tmp/console-oidc-values.yaml
   ```

## 6. Log in

Open `https://${CONSOLE_HOST}` in a browser, accept the self-signed certificate
warning, and sign in with the email and password you set. The console
auto-creates your account and organization on first login.

If the browser cannot reach the hostname, confirm the node IP is reachable from
your machine and that `nip.io` resolves it (`getent hosts "$CONSOLE_HOST"`). If
login bounces back to the login page, the most common cause is an `issuer`
mismatch: `console.oidc.issuerURL` must be byte-for-byte the Dex `issuer`, and the
console pod must be able to reach that URL.

## Where to go next

- Real sandbox forking requires `/dev/kvm` on the node (see What you need).
  Validate the engine with the smoke steps in `docs/quickstart.md` and the SDK.
- For a production bare-metal cluster, follow `talos-hetzner.md` and replace the
  single-user Dex with a real identity provider and trusted TLS.
- For the federated GitHub/Google Dex the chart ships, see the `dex.*` values in
  `deploy/charts/mitos/values.yaml`.
- For day-2 operations (upgrading, CRD schema changes, rollback, backup,
  uninstall), see `lifecycle.md`.
