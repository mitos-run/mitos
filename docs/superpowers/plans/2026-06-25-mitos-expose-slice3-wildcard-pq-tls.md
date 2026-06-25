# Mitos Expose Slice 3: wildcard cert, post-quantum guardrail, and deploying the proxy

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax.

**Goal:** Serve the edge proxy over a real wildcard TLS certificate with post-quantum key exchange, and deploy the proxy in the Helm chart so the controller route-sync loop reaches a running proxy in-cluster.

**Architecture:** A `WildcardProvider` loads an operator-provided `*.<expose-domain>` certificate and key via `tls.LoadX509KeyPair` and serves it for every SNI; `cmd/preview-proxy` selects it when `--tls-cert`/`--tls-key` are set, falling back to the existing `SelfSignedProvider`. The proxy's TLS config is extracted to `preview.ServerTLSConfig` so a post-quantum guardrail test can assert the server negotiates X25519MLKEM768 (Go 1.26 default with nil `CurvePreferences`). The Helm chart gains an `expose` block that deploys the proxy Deployment and Service, mounts the signing secret and admin token, mounts the wildcard cert Secret, optionally issues it via a cert-manager Certificate (ACME DNS-01), and wires the controller's `--expose-proxy-admin-url` and `EXPOSE_PROXY_ADMIN_TOKEN` to it.

**Tech Stack:** Go crypto/tls, Helm, cert-manager.

## Global Constraints
- Go 1.26 (PQ X25519MLKEM768 is the default group when `CurvePreferences` is nil; do NOT set `CurvePreferences` on any server TLS config, or PQ silently regresses).
- No em/en dashes anywhere including comments, docs, YAML, commit messages; only `.` `,` `;` `:` and ASCII hyphen.
- TLS cert and key file paths are config; the private key content is never logged. The signing secret and admin token remain bearer credentials (env, never argv, never logged).
- Honest claim scope: post-quantum KEY EXCHANGE (hybrid X25519MLKEM768, harvest-now-decrypt-later confidentiality) ONLY. NEVER claim post-quantum certificates or authentication: the certificate signature stays classical (ECDSA/RSA). README and threat-model wording is confidentiality-only.
- Pick the wildcard-cert path, NOT on-demand CertMagic (the documented `CertMagicProvider` stays a non-compiled doc comment); this matches the design spec and avoids the unverifiable ACME-in-CIcode path.
- TDD; DCO sign-off; explicit-path staging; conventional commits. Lint both darwin and GOOS=linux for touched Go packages. Validate chart changes with `helm template` / `helm lint`.

---

### Task 1: WildcardProvider

A `CertProvider` that loads one cert+key pair and serves it for every SNI host (the cert is a wildcard `*.<expose-domain>`, so the browser match is the cert's job, not the provider's).

**Files:**
- Modify: `internal/preview/cert.go` (add `WildcardProvider` + `NewWildcardProvider`)
- Test: `internal/preview/cert_test.go`

**Interfaces:**
- Produces: `type WildcardProvider struct { cert tls.Certificate }` and `func NewWildcardProvider(certFile, keyFile string) (*WildcardProvider, error)` (wraps `tls.LoadX509KeyPair`), with `GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error)` returning the loaded cert for any SNI (rejects empty SNI like the self-signed provider, for parity).

- [ ] Step 1: Failing test `TestWildcardProviderServesLoadedCert`: generate a self-signed `*.example.com` cert+key to two temp files (use `crypto/ecdsa`+`x509.CreateCertificate`+`pem` in the test, or reuse a helper), call `NewWildcardProvider(certFile, keyFile)`, then `GetCertificate(&tls.ClientHelloInfo{ServerName: "openclaw.example.com"})` and assert it returns the loaded cert (compare the leaf DER). Add `TestWildcardProviderRejectsEmptySNI` (empty ServerName -> error) and `TestNewWildcardProviderMissingFile` (a nonexistent path -> error).
- [ ] Step 2: Run `go test ./internal/preview/ -run TestWildcardProvider -v`; confirm fail.
- [ ] Step 3: Implement in `cert.go`:
```go
// WildcardProvider serves a single operator-provided certificate (a wildcard
// *.<expose-domain> cert) for every SNI host. The certificate is loaded once at
// startup via tls.LoadX509KeyPair; matching the hostname to the wildcard is the
// certificate's job, performed by the client. This is the production and bare
// metal TLS path (a cert-manager-issued or operator-provided wildcard), distinct
// from the self-signed test provider and the documented on-demand CertMagic seam.
type WildcardProvider struct {
	cert tls.Certificate
}

// NewWildcardProvider loads the cert and key from disk. It returns an error if
// either file is missing or unparseable, so a misconfigured deployment fails
// closed at startup rather than serving a broken handshake.
func NewWildcardProvider(certFile, keyFile string) (*WildcardProvider, error) {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("preview: load wildcard cert: %w", err)
	}
	return &WildcardProvider{cert: cert}, nil
}

// GetCertificate returns the loaded wildcard certificate for any non-empty SNI.
func (p *WildcardProvider) GetCertificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	if hello == nil || hello.ServerName == "" {
		return nil, errors.New("preview: no SNI server name")
	}
	c := p.cert
	return &c, nil
}
```
- [ ] Step 4: Run, confirm pass; build `go build ./internal/preview/...`; lint both.
- [ ] Step 5: Commit `internal/preview/cert.go internal/preview/cert_test.go` with `feat(preview): WildcardProvider loads an operator wildcard cert`.

---

### Task 2: ServerTLSConfig extraction and the post-quantum guardrail

Extract the proxy's server TLS config into `internal/preview` so it is testable, and add a guardrail test that proves the server negotiates X25519MLKEM768.

**Files:**
- Modify: `internal/preview/cert.go` (add `ServerTLSConfig`)
- Test: `internal/preview/tls_pq_test.go`

**Interfaces:**
- Produces: `func ServerTLSConfig(cp CertProvider) *tls.Config` returning `&tls.Config{GetCertificate: cp.GetCertificate, MinVersion: tls.VersionTLS12}` with `CurvePreferences` left NIL (so Go's default, which leads with X25519MLKEM768, is used). A doc comment states the nil-CurvePreferences invariant and why.

- [ ] Step 1: Failing test `TestServerTLSConfigNegotiatesPostQuantum`: build a `SelfSignedProvider`, `cfg := ServerTLSConfig(provider)`, start a TLS listener with it (a real `tls.NewListener` over a `net.Listen`, or `httptest.NewUnstartedServer` with `.TLS = cfg` then `StartTLS`). Dial it with a client `tls.Config{InsecureSkipVerify: true, CurvePreferences: []tls.CurveID{tls.X25519MLKEM768}, MinVersion: tls.VersionTLS13}` (the client offers ONLY the PQ group). Assert the handshake SUCCEEDS (a server that did not offer X25519MLKEM768 would fail this client), and assert `conn.ConnectionState().CurveID == tls.X25519MLKEM768` (Go 1.25+ exposes the negotiated `CurveID`). Add a second assertion that `cfg.CurvePreferences == nil` (the load-bearing invariant: pinning curves would drop PQ). Name a clear failure message tying a failure to "post-quantum key exchange regressed; do not set CurvePreferences".
- [ ] Step 2: Run, confirm fail (`undefined: ServerTLSConfig`).
- [ ] Step 3: Implement `ServerTLSConfig` in `cert.go`:
```go
// ServerTLSConfig returns the TLS config the expose proxy serves with. It sets
// GetCertificate from the provider and a TLS 1.2 floor (TLS 1.3 is negotiated
// when the client supports it). It deliberately leaves CurvePreferences NIL so
// Go's default key-exchange preference applies, which on Go 1.24 and newer leads
// with the hybrid post-quantum group X25519MLKEM768 (FIPS 203 ML-KEM-768 plus
// X25519), giving harvest-now-decrypt-later confidentiality. DO NOT set
// CurvePreferences here: pinning the curve list silently drops the post-quantum
// default. The guardrail test in tls_pq_test.go fails if this regresses.
func ServerTLSConfig(cp CertProvider) *tls.Config {
	return &tls.Config{
		GetCertificate: cp.GetCertificate,
		MinVersion:     tls.VersionTLS12,
	}
}
```
- [ ] Step 4: Run, confirm pass; lint both. If `ConnectionState().CurveID` is unavailable in the toolchain, fall back to asserting only that the PQ-only client handshake succeeds (that alone proves server support) and note it in the test comment.
- [ ] Step 5: Commit `internal/preview/cert.go internal/preview/tls_pq_test.go` with `feat(preview): ServerTLSConfig with a post-quantum key-exchange guardrail`.

---

### Task 3: wire the cert flags into the proxy

`cmd/preview-proxy` selects the WildcardProvider when `--tls-cert`/`--tls-key` are set, else the SelfSignedProvider, and builds its server from `ServerTLSConfig`.

**Files:**
- Modify: `cmd/preview-proxy/main.go`
- Test: `cmd/preview-proxy/main_test.go` (a small provider-selection test if the package has a test harness; otherwise a build + manual-reasoning step)

**Interfaces:** consumes `NewWildcardProvider`, `NewSelfSignedProvider`, `ServerTLSConfig`.

- [ ] Step 1: Add flags `tlsCert := flag.String("tls-cert", "", "path to the wildcard TLS certificate (PEM); empty uses a self-signed cert")` and `tlsKey := flag.String("tls-key", "", "path to the wildcard TLS private key (PEM)")`. Select the provider:
```go
var certProvider preview.CertProvider
if *tlsCert != "" || *tlsKey != "" {
	if *tlsCert == "" || *tlsKey == "" {
		log.Fatal("preview-proxy: --tls-cert and --tls-key must be set together")
	}
	wp, err := preview.NewWildcardProvider(*tlsCert, *tlsKey)
	if err != nil {
		log.Fatalf("preview-proxy: %v", err)
	}
	certProvider = wp
	logger.Info("preview-proxy: serving the operator wildcard certificate", "cert", *tlsCert)
} else {
	ss, err := preview.NewSelfSignedProvider()
	if err != nil {
		log.Fatalf("preview-proxy: cert provider: %v", err)
	}
	certProvider = ss
	logger.Info("preview-proxy: serving self-signed certificates (no --tls-cert set; not browser trusted)")
}
```
- [ ] Step 2: Change `startServers` to build its `httpsSrv.TLSConfig` from `preview.ServerTLSConfig(cp)` instead of the inline literal, so the deployed proxy and the guardrail test share one config builder.
- [ ] Step 3: `go build ./cmd/preview-proxy/...`; `go vet`; lint both. If a `main_test.go` exists, add a test that `--tls-cert` without `--tls-key` is rejected; otherwise verify by reasoning and note it.
- [ ] Step 4: Commit `cmd/preview-proxy/main.go` (and any test) with `feat(preview-proxy): select the wildcard cert via --tls-cert/--tls-key`.

---

### Task 4: deploy the expose proxy in the Helm chart

Add the proxy Deployment and Service, the secrets and cert mounts, an optional cert-manager wildcard Certificate, and the controller wiring, all gated by `.Values.expose.enabled` (default false).

**Files:**
- Create: `deploy/charts/mitos/templates/expose-proxy.yaml`
- Modify: `deploy/charts/mitos/values.yaml` (an `expose:` block)
- Modify: `deploy/charts/mitos/templates/controller-deployment.yaml` (pass `--expose-proxy-admin-url` + `EXPOSE_PROXY_ADMIN_TOKEN` when `expose.enabled`)

**Interfaces:** none (deploy). Follow `gateway.yaml` for the Deployment/Service/labels/securityContext/imagePullSecrets idiom.

- [ ] Step 1: Add to `values.yaml`:
```yaml
expose:
  enabled: false
  image:
    repository: ghcr.io/mitos-run/preview-proxy
    tag: ""           # defaults to the chart appVersion via mitos.image
    pullPolicy: IfNotPresent
  replicas: 1
  domain: ""          # the expose domain, e.g. mitos.app; required when enabled
  service:
    type: ClusterIP   # or LoadBalancer
  ingress:
    enabled: false
    className: ""
    annotations: {}
  tls:
    # certManager.enabled issues a wildcard *.<domain> Certificate via the named issuer.
    certManager:
      enabled: false
      issuerRef:
        name: ""
        kind: ClusterIssuer
    # secretName is the TLS Secret mounted into the proxy (created by cert-manager
    # when certManager.enabled, or provided by the operator otherwise).
    secretName: mitos-expose-tls
  # signingSecretRef / adminTokenSecretRef name a Secret with keys the proxy and
  # controller share. If empty, the chart creates mitos-expose with random values.
  secretName: mitos-expose
  resources: {}
```
- [ ] Step 2: Create `expose-proxy.yaml` (gated `{{- if .Values.expose.enabled }}`): a Deployment for `cmd/preview-proxy` with args `--addr=:8443`, `--domain={{ .Values.expose.domain }}`, `--tls-cert=/tls/tls.crt`, `--tls-key=/tls/tls.key`; env `MITOS_PREVIEW_SECRET` and `MITOS_EXPOSE_ADMIN_TOKEN` from the `expose.secretName` Secret keys; a volume mounting the `expose.tls.secretName` Secret at `/tls` read-only; the same securityContext (runAsNonRoot, drop ALL, readOnlyRootFilesystem, seccomp RuntimeDefault) and `/healthz` probes as gateway.yaml. Then a Service (port 443 -> 8443, type from values) and an optional Ingress. Then, when `.Values.expose.tls.certManager.enabled`, a `cert-manager.io/v1` Certificate with `dnsNames: ["*.{{ .Values.expose.domain }}"]`, `secretName: {{ .Values.expose.tls.secretName }}`, and `issuerRef` from values. Add a fail-rendering guard (a `required` on `expose.domain` when enabled).
- [ ] Step 3: In `controller-deployment.yaml`, when `.Values.expose.enabled`, append `--expose-proxy-admin-url=https://mitos-expose.<namespace>.svc` (or the Service DNS) to the controller args and set `EXPOSE_PROXY_ADMIN_TOKEN` from the shared `expose.secretName` Secret key. Match the existing env/arg patterns in that file.
- [ ] Step 4: Validate: `helm template <chart> --set expose.enabled=true --set expose.domain=mitos.app --set expose.tls.certManager.enabled=true --set expose.tls.certManager.issuerRef.name=letsencrypt` renders without error and includes the Deployment, Service, the Certificate, and the controller args/env; `helm lint`. Also render with `expose.enabled=false` and confirm NO expose resources appear (default off). Run `helm template` with the chart's normal values to confirm no regression.
- [ ] Step 5: Commit `deploy/charts/mitos/templates/expose-proxy.yaml deploy/charts/mitos/values.yaml deploy/charts/mitos/templates/controller-deployment.yaml` with `feat(deploy): deploy the expose proxy with a wildcard cert and controller wiring`.

---

### Task 5: docs and threat-model delta

Document the TLS architecture honestly (PQ key exchange only) and update the threat-model.

**Files:**
- Modify: `docs/preview-urls.md` (the TLS section)
- Modify: `docs/threat-model.md` (the expose ingress TLS row)

- [ ] Step 1: `docs/preview-urls.md`: replace the self-signed-only TLS description with: the proxy serves a wildcard `*.<expose-domain>` cert (operator-provided or cert-manager ACME DNS-01) via `--tls-cert`/`--tls-key`, self-signed as the default fallback; TLS terminates at the Go proxy so post-quantum key exchange (hybrid X25519MLKEM768, Go 1.24+ default) protects confidentiality against harvest-now-decrypt-later; the certificate signature stays classical (no post-quantum CA exists); the on-demand CertMagic path remains a documented non-compiled seam; the chart deploys the proxy gated by `expose.enabled`. No em/en dashes.
- [ ] Step 2: `docs/threat-model.md`: update the expose ingress TLS row: wildcard cert (single key, blast radius mitigated by short lifetimes/auto-rotation and key confinement to the proxy); PQ key exchange for confidentiality only (NOT authentication; classical cert signature); the guardrail test prevents a silent PQ regression; the proxy is now a deployed public ingress surface gated by `expose.enabled` and still sequenced behind the #194 review and #213 abuse envelope before untrusted public exposure. No em/en dashes.
- [ ] Step 3: Dash check both docs; commit `docs/preview-urls.md docs/threat-model.md` with `docs(expose): wildcard plus post-quantum TLS and the deployed proxy`.

---

## Self-review notes
- Coverage: WildcardProvider (Task 1), the PQ guardrail bound to the real server config (Task 2), the proxy cert-flag wiring (Task 3), the chart deploying the proxy with the wildcard cert and controller wiring (Task 4), honest docs (Task 5).
- Deferred: on-demand CertMagic ACME (documented seam, not compiled); the auth ladder (slice 4); `mitos workspace serve` (slice 5); the harness recipe (slice 6); the per-sandbox expose concurrency cap.
- Honesty invariant: PQ key exchange / confidentiality ONLY; never claim PQ certificates or authentication. The guardrail test asserts the curve, not the cert.
- The `CurvePreferences == nil` invariant is the load-bearing PQ guarantee; Task 2's test asserts it and the doc comment warns against pinning curves.
