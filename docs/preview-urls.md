# Preview URLs: per-sandbox port exposure with auto-TLS (issue #126)

A preview URL exposes a port inside a running sandbox to the caller through a
single per-sandbox hostname, `<sandbox-id>.preview.<domain>`, served by a
controller-managed reverse proxy with automatic TLS. It is the mitos equivalent
of E2B `sandbox.getHost(port)` and Daytona signed, expiring preview URLs: one
internet-facing entrypoint fronting many ephemeral per-sandbox backends,
Kubernetes-native, with the same per-sandbox token gate as the sandbox API.

This document describes the verifiable core that ships today (the routing, the
signed/expiring URL scheme, the route table and its GC, the SDK `get_host`
call) and the parts that are wired behind an interface for a maintainer to
enable with a real domain (the CertMagic on-demand ACME path).

## What ships in this slice

- `internal/preview`: the signer (mint + verify signed expiring URLs), the host
  parser, the route table with GC, the reverse proxy, and the `CertProvider`
  interface with a self-signed test provider.
- `cmd/preview-proxy`: the standalone proxy binary. One HTTPS entrypoint that
  resolves the vhost, verifies the token, looks up the backend, and proxies.
- `sandbox-server` `POST /v1/preview`: mints a signed preview URL for a sandbox
  port (the signing secret lives on the server, never in the SDK).
- Python SDK `DirectSandbox.get_host(port)` and the E2B shim
  `Sandbox.get_host(port)`: return the signed URL.

## Request flow

```
caller ──HTTPS──▶  preview-proxy (one entrypoint)
                     1. parse  <sandbox-id>.preview.<domain>  ─▶ sandbox id
                     2. verify signed expiring token (HMAC, not expired)
                     3. bind token to the sandbox in the host (reject cross-sandbox)
                     4. look up backend IP:port in the route table (Ready claims)
                     5. attach the per-sandbox bearer (the :9091 gate),
                        strip the preview token from the forwarded query
                     6. reverse-proxy to the sandbox backend port
```

A failure at any step is terse and never echoes the token: unknown vhost or no
route is `404`, missing/invalid/expired token is `401`, a token for a different
sandbox is `403`, an unreachable backend is `502`.

## The signed, expiring URL scheme

A preview URL is `https://<sandbox-id>.preview.<domain>/?token=<token>`. The
token is a detached HMAC over a compact JSON payload:

```
payload = base64url(json{ "s": sandboxID, "p": port, "e": expiryUnix })
tag     = base64url( HMAC-SHA256( serverSecret, "mitos-preview-v1\0" || payload ) )
token   = payload + "." + tag
```

`Verify` recomputes the tag, compares it in CONSTANT TIME (`crypto/subtle`),
then rejects the token if `now` is after the embedded expiry. The properties
that matter, each unit-tested in `internal/preview/sign_test.go`:

- **Never accept after expiry.** A token one second past its expiry, and a token
  checked far in the future, both fail. The expiry boundary is inclusive.
- **Never accept tampered.** Flipping any byte of the payload or the tag fails
  the constant-time compare; garbage fails to parse.
- **Never accept wrong key.** A token minted under a different server secret
  fails to verify.
- **Bound to one sandbox and one port.** The proxy additionally requires the
  verified token to name the sandbox in the vhost, so a leaked URL cannot be
  replayed against another sandbox.

### Why not a captoken

mitos already has macaroon-style attenuated capability tokens
(`internal/captoken`, issue #25). A preview token needs no attenuation chain,
only a single expiring binding of `(sandbox, port)`, so a focused signer keeps
the scheme small and auditable. It reuses the SAME standard-library HMAC-SHA256
and constant-time-compare core as captoken and the W4 S3 SigV4 signer
(`internal/workspace/s3client.go`); no new crypto is invented.

### Secret handling

The server secret (`MITOS_PREVIEW_SECRET`, at least 16 bytes) and every minted
token are BEARER CREDENTIALS. They are never logged, never put in an error,
condition, or event, and never written to a host path. The proxy logs the
sandbox id and the HTTP status only; the signing path logs nothing. The proxy
strips the `token` query parameter before forwarding, so the sandbox backend
never sees the preview bearer.

## Route table and GC

The route table maps `<sandbox-id>` to its backend `IP:port` and the
per-sandbox token. It is built ONLY from Ready claims: a claim with
`Status.Phase==Ready` and a non-empty `Status.Endpoint` becomes a route.
`RouteTable.Sync(states)` reconciles the table to exactly the current Ready set,
so a sandbox that leaves the Ready set (terminate) has its route REAPED on the
next sync and is immediately unroutable (`404`). The table logic and the
add-on-ready / remove-on-terminate GC are unit-tested against an injectable
claim source (`ClaimState`); the controller wiring that feeds it from the live
claim watch is a thin documented follow-up.

## TLS

TLS issuance is wired behind a single interface so the routing and signing core
never depend on a working ACME path:

```go
type CertProvider interface {
    GetCertificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error)
}
```

The signature matches `tls.Config.GetCertificate`, so a provider installs
directly. This slice ships `SelfSignedProvider`, which mints a self-signed cert
per SNI host on first request (the same per-hostname, mint-on-first-request
shape as on-demand TLS). A self-signed cert is NOT browser-trusted and is never
a production substitute.

### Production: CertMagic on-demand TLS (follow-up)

Real ACME issuance needs a public domain, a `*.preview.<domain>` DNS record (or
per-host A records), and a publicly reachable endpoint, none of which exist in
CI, so the heavy `certmagic` dependency is NOT compiled in this slice. A
maintainer with a domain implements `CertProvider` over CertMagic (the Go ACME
library behind Caddy), embedded natively; the documented adapter lives in the
`CertMagicProvider` doc comment in `internal/preview/cert.go`. The on-demand
`DecisionFunc` MUST consult the route table so the proxy only asks the CA for a
hostname that resolves to a live Ready sandbox; this caps ACME rate-limit
exposure and stops the proxy being a CA amplifier for arbitrary SNI.

### Bare metal

Bare metal is a first-class target, so the TLS story does not require a public
CA:

- **Self-hosted ACME.** Point CertMagic at an internal ACME server (for example
  `step-ca`) instead of Let's Encrypt; the same `CertProvider` adapter applies.
- **Provided wildcard cert.** Load a maintainer-provided `*.preview.<domain>`
  certificate with `tls.LoadX509KeyPair` and serve it from a `CertProvider` that
  returns it for every matching SNI host. No ACME at all.

## SDK usage

```python
import mitos

sb = mitos.create("python")
url = sb.get_host(8080)          # signed, expiring preview URL for port 8080
# url == "https://<id>.preview.<domain>/?token=..."
```

`get_host(port)` asks the server to mint the URL (`POST /v1/preview`) and
returns it; the signing secret never leaves the server. A server that does not
expose the preview proxy returns a typed `501`. The E2B compatibility shim
(`mitos.e2b.Sandbox.get_host`, issue #206) delegates to this same method, so an
E2B script's `sandbox.get_host(port)` works unchanged.

## Operating the proxy

```bash
export MITOS_PREVIEW_SECRET=<at least 16 random bytes, kept secret>
preview-proxy --domain example.com --addr :8443
```

The proxy serves HTTPS using the `CertProvider` (self-signed by default in this
slice). `--http-addr` adds a plaintext listener for testing or for running
behind a separate TLS terminator. The route table is populated from Ready
claims by the controller wiring follow-up.

## Production gate

This ingress adds a public attack surface. It is NOT cleared for production
tenants until the external security review (issue #194) covers it. Edge rate
limiting and an SNI/connection cap are documented follow-ups, sequenced with the
#213 abuse-control envelope. See docs/threat-model.md section 7c.
