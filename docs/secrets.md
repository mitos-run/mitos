# Secrets management (multi-tenant)

This is the consolidated, operator-facing page for how Mitos handles tenant
secrets, especially in a multi-tenant cluster. It pulls together what was
scattered across `docs/threat-model.md`, `docs/encryption.md`, and
`docs/fork-correctness.md`, and is explicit about what is implemented versus
planned. Every claim cites a repository file and line so it can be spot-checked
against the code; this is the no-unverified-claims rule from `CLAUDE.md`.

This document does NOT duplicate the encryption-at-rest design or the threat
model. For the full at-rest key-custody design see `docs/encryption.md`; for the
per-boundary security status see `docs/threat-model.md`; for fork-correctness
hazards see `docs/fork-correctness.md`.

## 1. The model in one paragraph

A tenant secret is never baked into a snapshot. It is resolved from a Kubernetes
Secret at claim time, delivered to the node over a mutually-authenticated TLS
gRPC channel, injected into the guest only AFTER the microVM is restored, and
held there ONLY in the guest agent's in-memory environment map: never written to
the `/workspace` disk, never on the Firecracker boot args, never on the VM
config socket. The microVM (KVM plus Firecracker) is the isolation boundary
between tenants, and on the default husk path each VM runs in its own
unprivileged, PSA-restricted pod that IS the per-VM boundary
(`docs/threat-model.md:141-154`). When a sandbox is forked, its platform
credential (the sandbox-API bearer token) is freshly reissued, not inherited,
and a fork that would duplicate tenant secret VALUES is rejected by default
unless the operator explicitly opts in (`internal/controller/sandboxfork_controller.go:151-168`).
At-rest encryption of the template snapshot is opt-in and uses a per-template
key the husk pod never sees (`docs/encryption.md:130-134`).

## 2. Secret lifecycle

### Declared

A tenant declares secrets as references to existing Kubernetes Secrets, not as
inline values. In the Python SDK, `create(secrets=...)` takes a map of env-var
name to a `(secret_name, secret_key)` tuple
(`sdk/python/mitos/client.py:216`, `sdk/python/mitos/client.py:226`), which the
SDK renders into the `Sandbox.spec.secrets` list as `secretRef` entries
(`sdk/python/mitos/client.py:252-260`). The CRD field is `SecretMount`, carrying
a `SecretRef` (a `corev1.SecretKeySelector`) and the target `EnvVar`
(`api/v1/types.go`).

### Resolved

At claim time the controller reads the referenced Kubernetes Secret and extracts
the named key into an in-memory map; a missing Secret or missing key fails the
reconcile rather than proceeding without the secret
(`internal/controller/sandboxclaim_controller.go:1315-1344`). The design is
deliberate: pools snapshot BEFORE secrets exist, so the secret is resolved per
claim and never present in the shared template
(`docs/threat-model.md:961`).

### Delivered

The controller hands the resolved values to forkd in the `Fork` gRPC call
(`internal/daemon/server.go:231`, `internal/daemon/server.go:245-246`). forkd
delivers them into the guest only AFTER the VM is restored, over the vsock
control channel, in `deliverConfig`: it registers the guest agent, runs the
fork-correctness `NotifyForked` handshake, then sends `Configure` with the
env and secrets (`internal/daemon/server.go:291-320`). Delivery is strict when
secrets are present: if `Configure` fails with secrets set, the fork errors and
the VM is reaped, because a sandbox that reports Ready without its secrets is a
lie (`internal/daemon/server.go:262`, `internal/daemon/server.go:314-317`). On
the husk path the same env and secrets ride the mTLS control channel in the
`ActivateRequest` and are delivered after the restore handshake, mirroring
`deliverConfig` (`internal/husk/control.go:29-32`,
`internal/husk/control.go:58-59`). Resolved secret values transit the
mTLS-protected controller-to-forkd channel as shipped; they are plaintext on the
wire only in flag-less dev deployments, where forkd warns loudly
(`docs/threat-model.md:961`).

### Used

Inside the guest, the agent stores the delivered env and secrets in a single
in-memory map, `configuredEnv`, guarded by a mutex; the values are never logged
(`guest/agent/main.go:32-44`, `guest/agent/main.go:254-269`). Each `exec` copies
that map into the child process environment (`guest/agent/main.go:289-294`). The
map lives in the guest process memory only: it is never persisted to the guest
filesystem, and specifically never written under `/workspace`
(`guest/agent/tardir.go:18-24`).

### Destroyed

A secret's lifetime is the VM's lifetime. The guest holds it only in process
memory (`guest/agent/main.go:32-33`), so VM teardown (the sandbox being reaped or
its claim TTL expiring) discards it with the VM's memory. The sandbox-API bearer
token is held in a controller-owned Secret that is owner-referenced to the claim
or fork, so Kubernetes garbage-collects it when the owner is deleted
(`internal/controller/token_secret.go:57-79`).

## 3. Multi-tenant isolation guarantees

Each guarantee below is the mechanism that keeps one tenant's secrets from
reaching another, with its citation.

- **The microVM boundary.** The isolation boundary between tenants is the microVM
  (KVM plus Firecracker), and on the default husk path each VM runs in its own
  unprivileged, capability-dropped, PSA-restricted pod that is itself the per-VM
  boundary (its own uid, netns, and cgroup); per-VM isolation comes from
  one-VM-per-unprivileged-pod plus the microVM
  (`docs/threat-model.md:141-154`, `docs/threat-model.md:167-174`). A guest that
  escaped the microVM on the husk default lands with no root, no Linux
  capabilities, no privilege-escalation path, and only a read-only base-image
  mount (`docs/threat-model.md:230-235`).

- **The per-sandbox bearer token.** Each sandbox gets its own API bearer token,
  minted from 32 bytes of `crypto/rand` (`internal/controller/token_secret.go:46-55`),
  stored in a Secret owner-referenced to the claim or fork so it is GC'd with it
  (`internal/controller/token_secret.go:62-79`). The token is a secret value:
  never logged, never in status, conditions, or events
  (`internal/controller/token_secret.go:46-48`,
  `internal/controller/token_secret.go:74-76`). On the husk path the stub gates
  the in-pod sandbox HTTP API on this token, so only a caller presenting it can
  reach the activated VM (`internal/husk/control.go:40-44`).

- **Token reissue on fork (no inheritance by default).** A fork mints a FRESH
  bearer token; the source's token never opens the fork
  (`internal/controller/sandboxfork_controller.go:238-242`). This is enforced and
  tested as a fork-correctness mitigation
  (`docs/fork-correctness.md:16`, `docs/fork-correctness.md:270-286`).

- **Secret-inheritance default-deny gate.** Because a live fork duplicates guest
  memory (and therefore any delivered secret VALUES), a fork of a
  secret-holding source is REJECTED by default with a typed `Rejected`/`SecretInheritanceDenied`
  condition; proceeding requires `spec.secretInheritance: inherit`, and the
  opt-in is recorded as an audit condition
  (`internal/controller/sandboxfork_controller.go:151-183`). The CRD field and its
  default-deny semantics are documented on the type
  (`api/v1/types.go`).

- **Workspace credential-path exclusion.** When a bound sandbox's `/workspace` is
  persisted into a committed revision, the dehydrate strips conventional
  credential paths so a captured revision never carries credential material:
  `.netrc`, `.git-credentials`, `.ssh`, `.aws`, `.config/gh`, and `.npmrc`
  (`internal/controller/workspace_binding.go:147-159`). This is defense in depth:
  secret VALUES live in the guest's in-memory env, not on `/workspace`, but the
  exclusion guards against a careless agent writing a token to one of these paths
  (`internal/controller/workspace_binding.go:147-151`). The guest tar transfer is
  itself confined to `/workspace` and refuses any path outside it, so the bulk
  transfer can never reach the guest's in-memory secret state
  (`guest/agent/tardir.go:18-24`, `guest/agent/tardir.go:28-37`).

## 4. The in-guest self-service socket carries names, never values

The in-VM workload (and the `mitos.guest` SDK) can read its own identity and
budget from a unix socket advertised via `MITOS_SOCKET`, with no network egress
(`guest/agent/selfservice.go:13-23`). The handler reads from the same
`configuredEnv` map that holds the claim-time env and secrets, but it whitelists
the keys it surfaces: it only returns the non-secret `MITOS_` identity and budget
keys, never a secret VALUE (`guest/agent/selfservice.go:45-58`). The socket env
variables forkd advertises are a NAME and a path, never a secret value
(`internal/daemon/server.go:305-312`, `internal/daemon/server.go:323-335`).

## 5. Encryption at rest and key custody

Encryption at rest is opt-in behind forkd's `--enable-encryption` flag; with the
flag off, snapshots are plaintext on disk exactly as before
(`docs/encryption.md:14-15`, `cmd/forkd/main.go:98`). When enabled, each template
is built inside its own per-template LUKS2 / dm-crypt container, so the bytes on
disk are ciphertext, and deletion is crypto-shredding (wiping the LUKS keyslots),
not a large overwrite (`docs/encryption.md:23-35`, `docs/encryption.md:70-81`).

Key custody uses envelope encryption. The controller generates a 32-byte
data-encryption key (DEK) with `crypto/rand`, wraps it with a key-encryption key
(KEK) via the KMS, zeroizes the plaintext DEK, and stores only the WRAPPED DEK
plus the non-secret KEK id in a `<template>-enc-key` Secret owner-referenced to
the template (`docs/encryption.md:135-152`). Where the key currently lives:

- **etcd holds only the wrapped DEK** (plus the non-secret KEK id), never the
  plaintext DEK (`docs/encryption.md:177-182`).
- **The KEK is the trust anchor.** For the shipped local provider it is an
  AES-256 key loaded by PATH from a Secret-mounted file (`--kek-file` on both the
  controller and forkd), never in argv, never logged
  (`docs/encryption.md:184-189`, `cmd/controller/main.go:89`, `cmd/forkd/main.go:99`).
- **The husk pod never sees the encryption key.** The per-template encryption key
  goes to forkd, which serves the pre-decrypted snapshot via dm-crypt; the key
  never enters the husk pod (`docs/threat-model.md:640`, `docs/encryption.md:132-134`).
- **The node data disk is not trusted.** Neither the plaintext nor the wrapped
  DEK is written there; only ciphertext and the LUKS structure are on disk
  (`docs/encryption.md:190-192`).
- **The plaintext DEK exists only briefly in forkd memory** during a container
  open or create, and is zeroized immediately after
  (`docs/encryption.md:169-173`).

Encryption fails closed: forkd refuses to start under `--enable-encryption`
without `--kek-file`, the encryption RPC requires mTLS, and an unwrap failure or
a missing wrapped DEK refuses the operation rather than running unencrypted
(`docs/encryption.md:233-236`, `cmd/forkd/main.go:98`, `docs/encryption.md:165-168`).

## 6. What is NOT done yet (known gaps)

Stated honestly so operators do not over-trust the current posture:

- **No production posture for untrusted multi-tenant code yet.** The threat model
  is explicit: do not run untrusted multi-tenant workloads in production on this
  project yet, and no external security review has happened
  (`docs/threat-model.md:13-16`, `docs/threat-model.md:89-92`).
- **No cloud KMS / external secrets-manager integration ships today.** Envelope
  encryption ships with the LOCAL AES-256-GCM KEK provider only; AWS KMS, GCP
  KMS, and HashiCorp Vault Transit are interface-only follow-ups with no cloud
  SDK added yet (`docs/encryption.md:237-240`, `docs/encryption.md:307-309`). The
  HSM-backed key-custody plan is tracked in
  `docs/superpowers/plans/2026-06-13-hsm-kms-key-custody.md`
  (`docs/superpowers/plans/2026-06-13-hsm-kms-key-custody.md:1`). There is no
  integration with an external secrets manager (Vault, cloud secret stores) for
  delivering TENANT secrets; tenant secrets are resolved from Kubernetes Secrets
  (`internal/controller/sandboxclaim_controller.go:1315-1344`).
- **Tenant-secret reissue on fork is not yet implemented.** The PLATFORM
  credential (the bearer token) is reissued per fork, but revoke-and-reissue of
  TENANT secret VALUES over vsock (so a fork could safely inherit FRESH secrets
  instead of being rejected) is a documented residual: static Kubernetes Secret
  values have no upstream to revoke, and
  capability-token per-fork attenuation lands with the runtime wiring
  (`docs/fork-correctness.md:16`, `docs/fork-correctness.md:270-286`). The
  default-deny gate (section 3) is what closes the hazard today.
- **In-memory key exposure to a node-root attacker.** While an encrypted
  container is open, the DEK is necessarily in forkd's process memory to serve
  I/O; a node-memory dump by a root attacker yields it. Full mitigation needs HSM
  key custody; zeroize-on-close is the current partial mitigation
  (`docs/encryption.md:319-323`).
- **CA private key in a namespace Secret.** The control-plane mTLS CA private key
  lives in a namespace Secret readable by namespace secret-readers, and there is
  no certificate rotation yet for the controller-to-forkd path
  (`docs/threat-model.md:756`). The CA private key (`ca.key`) is never replicated
  into pool namespaces; only the public `ca.crt` is
  (`docs/threat-model.md:966-968`).
- **Controller RBAC for Secrets is broad by default.** The namespaced-Secrets
  narrowing is shipped but opt-in (`controller.namespacedSecretsRBAC=true`); by
  default the controller holds a cluster-wide Secrets grant, so a stolen
  controller token can read Secrets cluster-wide
  (`docs/threat-model.md` Controller RBAC for Secrets row). The controller is the
  trust anchor for secret delivery: a compromised controller can deliver secrets
  to any pod it can reach (`docs/threat-model.md:248-253`).
- **CAS chunk store is not encrypted.** The content-addressed snapshot store is
  not encrypted today; only per-template containers are
  (`docs/encryption.md:317-318`).
- **forkd-side container shred on template GC is deferred.** Deleting a template
  GC's the key Secret (crypto-shredding at the Kubernetes level), but the
  controller does not yet send a `DeleteTemplate` RPC, so the node-side encrypted
  container is reclaimed only by node data-dir lifecycle until that wiring lands
  (`docs/encryption.md:242-249`, `docs/encryption.md:303-305`).

## 7. For operators

- **Enable encryption at rest for production.** Run forkd with
  `--enable-encryption` and supply `--kek-file` on both the controller and forkd
  (a Secret-mounted 32-byte AES-256 KEK). forkd refuses to start under
  `--enable-encryption` without it, and the encryption RPC requires mTLS
  (`cmd/forkd/main.go:98-99`, `cmd/controller/main.go:89`,
  `docs/encryption.md:233-236`).
- **Always deploy with mTLS.** forkd fails closed without mTLS on its gRPC
  surface; `--allow-insecure-grpc` exists for local dev only and is never set by
  the shipped DaemonSet (`cmd/forkd/main.go:82`). Provide `--tls-cert`,
  `--tls-key`, and `--tls-ca` (`cmd/forkd/main.go:79-81`). Tenant secret values
  are plaintext on the wire only in flag-less dev deployments
  (`docs/threat-model.md:961`).
- **Where keys live.** The wrapped per-template DEK and the non-secret KEK id are
  in a `<template>-enc-key` Kubernetes Secret; the KEK is a Secret-mounted file
  referenced by `--kek-file`; per-sandbox bearer tokens are in
  `<sandbox>-sandbox-token` Secrets owner-referenced to the claim or fork
  (`docs/encryption.md:135-152`, `internal/controller/token_secret.go:16-18`,
  `internal/controller/token_secret.go:62-79`).
- **Rotation.** Deleting a `SandboxPool` GC's its key Secret, crypto-shredding
  the at-rest key (`docs/encryption.md:198-211`). Per-sandbox tokens rotate with
  the sandbox lifecycle (GC'd with the owner,
  `internal/controller/token_secret.go:62-79`). KEK rotation with DEK re-wrap, and
  DEK rotation with container re-encryption, are documented follow-ups, not yet
  implemented (`docs/encryption.md:310-314`).
- **Narrow controller Secrets RBAC.** Set
  `controller.namespacedSecretsRBAC=true` so the cluster-wide Secrets grant is
  removed and the controller reaches Secrets only in the controller namespace and
  adopted pool namespaces (`docs/threat-model.md` Controller RBAC for Secrets
  row).
- **Restrict the secret-holding fork path.** Leave `spec.secretInheritance` at
  its `reissue` default so forks of secret-holding sandboxes are rejected rather
  than silently duplicating tenant secrets into a child
  (`internal/controller/sandboxfork_controller.go:151-168`,
  `api/v1/types.go`).
