# Single-user Dex for the Mitos console

> WARNING: QA and first-run ONLY. NOT FOR PRODUCTION. NOT MULTI-TENANT.
>
> This example gates the Mitos console behind exactly ONE static
> username/password so you can log in without standing up GitHub/Google OAuth
> apps or a full identity provider. It uses in-memory storage, plaintext HTTP
> for the issuer, a self-signed TLS certificate for the console, and a single
> hardcoded credential. None of that is acceptable for a shared, internet-facing,
> or multi-user deployment.
>
> This does NOT provide tenant isolation. Mitos sandboxes are microVMs, not
> pods; nothing here governs sandbox isolation. It only gates who can log into
> the console. For more than one user, or anything production, run the chart's
> federated Dex (`dex.enabled`, GitHub/Google) or point `console.oidc.*` at a
> real issuer (Keycloak, Okta, Auth0, Google).

## What this gives you

A minimal [Dex](https://dexidp.io) deployment with a single static-password user,
wired to the Mitos console's OIDC login flow. After applying it you can open the
console in a browser and sign in with one email and password.

## How it fits the console's OIDC contract

The console (`cmd/console`) reads its OIDC settings from env the Helm chart sets
from `console.oidc.*`:

| Chart value | Console env | Must equal |
| --- | --- | --- |
| `console.oidc.issuerURL` | `MITOS_CONSOLE_OIDC_ISSUER` | the Dex `issuer` in `dex.yaml` |
| `console.oidc.clientID` | `MITOS_CONSOLE_OIDC_CLIENT_ID` | the Dex static client `id` in `dex.yaml` |
| `console.oidc.clientSecretRef` | `MITOS_CONSOLE_OIDC_CLIENT_SECRET` (key `client-secret`) | the Secret Dex also reads its client secret from |
| `console.oidc.redirectURL` | `MITOS_CONSOLE_OIDC_REDIRECT_URL` | a `redirectURIs` entry on the Dex static client |

The console requests the `email` and `profile` scopes and REJECTS any login whose
email is not verified. Dex's password-DB connector (`enablePasswordDB: true`)
marks the static user's email as verified, so login succeeds. On first successful
login the console auto-creates the account and its organization.

Two URL rules matter and are easy to get wrong:

- The `issuer` URL must be byte-for-byte identical from the browser AND from
  inside the console pod (OIDC discovery compares the issuer string exactly). A
  `nip.io` hostname built from your node IP satisfies both. Dex runs over plain
  HTTP so the console pod can discover it without a trusted TLS certificate.
- The console must be served over HTTPS, because it sets its session and CSRF
  cookies with the `Secure` flag (the browser drops them otherwise). This example
  gives the console a self-signed certificate so login works on a first install
  with no cert-manager. The browser shows a certificate warning you click
  through; that is expected for QA and unacceptable for production.

## Files

- `dex.yaml`: Dex ConfigMap, Deployment, Service, and a plain-HTTP Ingress. One
  static-password user. No external connectors.
- `console-ingress.yaml`: HTTPS Ingress for the `mitos-console` Service (the one
  the chart renders), using a self-signed certificate you create.
- `console-oidc-values.yaml`: Helm values overlay that points `console.oidc.*` at
  this Dex, turns the chart's federated Dex off, and turns the chart's non-TLS
  console Ingress off.
- `kustomization.yaml`: lets you render or apply `dex.yaml` + `console-ingress.yaml`
  as a set.

## Prerequisites

- A running Mitos install (the Helm chart) in the `mitos` namespace. See
  `docs/platforms/k3s-quickstart.md` for a from-scratch k3s walkthrough that
  uses this example.
- An Ingress controller. On k3s, Traefik is installed by default.
- `kubectl`, `helm`, `htpasswd` (from `apache2-utils` / `httpd-tools`), and
  `openssl` on your workstation.

## Step 1: choose hostnames

Pick hostnames that resolve to your cluster from both your browser and the
console pod. On a single node, `nip.io` works (it resolves `<anything>.<ip>.nip.io`
to `<ip>`). Export the node IP and derive the two hostnames:

```bash
export NODE_IP=$(kubectl get nodes -o jsonpath='{.items[0].status.addresses[?(@.type=="InternalIP")].address}')
export DEX_HOST="dex.${NODE_IP}.nip.io"
export CONSOLE_HOST="console.${NODE_IP}.nip.io"
echo "Dex:     http://${DEX_HOST}"
echo "Console: https://${CONSOLE_HOST}"
```

Replace `dex.example.com` with `$DEX_HOST` and `console.example.com` with
`$CONSOLE_HOST` in `dex.yaml`, `console-ingress.yaml`, and
`console-oidc-values.yaml` before applying. Edit the files, or stamp them with
`sed` into copies:

```bash
for f in dex.yaml console-ingress.yaml console-oidc-values.yaml; do
  sed -e "s/dex.example.com/${DEX_HOST}/g" \
      -e "s/console.example.com/${CONSOLE_HOST}/g" \
      "$f" > "/tmp/${f}"
done
```

## Step 2: set your password

The committed bcrypt hash is a deliberately UNUSABLE placeholder, so you MUST
generate your own. Generate a bcrypt hash for your chosen password:

```bash
# Prompts for the password twice, prints a bcrypt hash.
htpasswd -bnBC 10 "" 'your-strong-password' | tr -d ':\n' | sed 's/^\$2y\$/\$2a\$/'
```

Paste the output into `staticPasswords[0].hash` in `dex.yaml`, and set
`staticPasswords[0].email` to the address you will sign in as. The leading
`$2a$`/`$2y$` form is bcrypt; the `sed` normalizes the prefix Dex expects. Never
commit your real hash back to a shared repo.

## Step 3: create the shared client-secret Secret

The console's OIDC client secret and Dex's static-client secret are the SAME
value, read from one Secret (key `client-secret`). It is never inlined in any
manifest. Generate a random value and create the Secret:

```bash
kubectl create secret generic mitos-console-oidc \
  --namespace mitos \
  --from-literal=client-secret="$(openssl rand -hex 32)"
```

## Step 4: create the self-signed console TLS Secret

```bash
openssl req -x509 -newkey rsa:2048 -nodes -days 365 \
  -keyout /tmp/console.key -out /tmp/console.crt \
  -subj "/CN=${CONSOLE_HOST}" \
  -addext "subjectAltName=DNS:${CONSOLE_HOST}"

kubectl create secret tls mitos-console-tls \
  --namespace mitos \
  --cert=/tmp/console.crt --key=/tmp/console.key
```

## Step 5: apply Dex and the console Ingress

```bash
kubectl apply -n mitos -f /tmp/dex.yaml -f /tmp/console-ingress.yaml
# or, if you edited the files in place:
# kubectl apply -k examples/single-user-dex
```

## Step 6: wire the console and roll it out

```bash
helm upgrade mitos deploy/charts/mitos -n mitos \
  --set namespace.create=false \
  -f /tmp/console-oidc-values.yaml
```

(Use the same chart reference you installed with: a checkout path, `mitos/mitos`,
or `oci://ghcr.io/mitos-run/charts/mitos`.)

## Step 7: log in

Open `https://${CONSOLE_HOST}` in a browser, accept the self-signed certificate
warning, and sign in with the email and password from Step 2. The console
auto-creates your account and organization on first login.

## Rotating or removing the user

- Change the password: regenerate the hash (Step 2), edit `dex.yaml`, re-apply,
  and restart Dex: `kubectl rollout restart deploy/mitos-dex-single-user -n mitos`.
- Remove single-user login: delete the Dex resources
  (`kubectl delete -k examples/single-user-dex`), the two Secrets, and re-run
  `helm upgrade` without `console-oidc-values.yaml`.

## Validation status

| Item | Status |
| --- | --- |
| Manifests parse as valid YAML | VALIDATED locally |
| `kubectl kustomize examples/single-user-dex` renders | VALIDATED locally |
| OIDC env/value names match what `cmd/console` reads | VERIFIED against source |
| End-to-end login on a live cluster | NOT verified here; requires a cluster |
