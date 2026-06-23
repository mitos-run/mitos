# Mitos Console — design spec

Date: 2026-06-22
Status: design (brainstorm output). Implementation plan is a follow-up.
Tracks: fills the open "decide stack and hosting; scaffold the app" task in
[#214](https://github.com/mitos-run/mitos/issues/214), under the
[#208](https://github.com/mitos-run/mitos/issues/208) hosted-SaaS epic. Proposes
two net-new sub-issues (see §11).

## Summary

The Mitos repo already has the SaaS spine: a public API gateway (`cmd/gateway`,
#210), an org-scoped backend-for-frontend (`cmd/console`, #214) that joins keys,
usage/cost, billing, live sandboxes, templates, members and audit behind tested
cross-tenant isolation, plus `internal/saas/{account,billing,console,gateway,
onboarding,quota,session,keys}` and a Helm chart that already ships an
operational Grafana dashboard. The deliberate gap (an explicit open task in
#214) is the **human dashboard UI** and the few backend seams it needs.

This spec designs that dashboard so that **the hosted SaaS and any self-hosted
install run the same binary and the same UI bundle**, differing only by a
runtime capabilities document the server advertises. It also designs first-class,
provider-backed **secret management** (default Kubernetes, recommended external
provider OpenBao) and a **proof surface** that makes Mitos's Pareto position
visible and verifiable in-product.

## Goals

- One artifact: self-hosted and hosted run the identical console image and SPA
  bundle. No build-time edition fork.
- On-brand: the dashboard renders in the website's "Fluorescence" design system,
  shared via a versioned `@mitos/brand` package.
- Competitor-grade breadth (match Daytona, beat both Daytona and E2B on live
  sandbox inspection), plus Mitos-only views (live fork tree, forkable
  workspaces, first-class secrets, proof panel).
- Per-tenant secret management with a pluggable provider backend.
- Honor the project's gating rules and integrity discipline (no unverified
  numbers; nothing flips to public untrusted multi-tenancy before the gates).

## Non-goals

- Tenant isolation hardening itself (owned by the multitenancy isolation track,
  the gating dependency named in #208).
- Engine-level metering correctness (#33).
- Re-implementing payment UI (we deep-link Stripe's Customer Portal).
- The operational Grafana dashboard (already shipped; unchanged).

## 1. Keystone: one artifact, capability-driven behavior

There is no `community` vs `hosted` build flag. One Go binary, one SPA bundle.
Behavior is driven by a runtime capabilities document.

```
GET /console/capabilities  →
{
  "edition":     "community" | "hosted",
  "billing":     false | true,     // Stripe plans/invoices/credits
  "signup":      false | true,     // self-serve org creation (see gate, §10)
  "teams":       true,             // members + roles (on in both)
  "idp":         "oidc",           // operator IdP vs our hosted IdP
  "orgSwitcher": false | true,     // self-host defaults to one org
  "secrets":     { "providers": ["kube"] | ["kube","openbao", ...] },
  "proof":       true,             // instrument/proof panel available
  "ownership":   "self-hosted" | "hosted"
}
```

The SPA fetches this at boot and conditionally mounts routes. The two editions
download the identical bundle from the identical binary; only the JSON differs.
This mirrors the BFF's existing "server decides, client renders" isolation
discipline and is also the **gate-enforcement mechanism** (§10): `signup` and
`billing` are server-controlled flags, not client choices.

## 2. Two front doors (existing split)

| Surface | Binary | Auth | Audience |
| --- | --- | --- | --- |
| Programmatic API (`sandbox.create`, SDK) | `cmd/gateway` (#210) | API key → org | agents / SDK |
| Human dashboard | `cmd/console` (#214) | OIDC session → org | people in a browser |

Both attach the **same org context** and sit above the internal mTLS /
per-sandbox-token plane (#4, `docs/superpowers/plans/2026-06-10-control-plane-auth.md`).
The console BFF already exists and is tested; this spec adds the UI, an OIDC
session in front, and makes three deferred seams real.

## 3. Packaging: SPA embedded in the Go binary

```
cmd/console (one Go binary, one image)
├── GET /*                     → embedded SPA (web/dist via go:embed)
├── GET/POST /console/*        → BFF JSON, org-scoped (exists today)
├── GET /console/capabilities  → edition + feature flags (§1)
└── /auth/*                    → OIDC login / callback / logout → session cookie
```

- `Dockerfile.console`: multi-stage — Node builds the SPA, Go embeds `web/dist`,
  output is one static binary. No Node at runtime, one image, one Deployment.
- This is the standard k8s product-console pattern (ArgoCD, Rancher, Longhorn,
  MinIO, Temporal UI, Vault UI). Note the chart ships **two distinct dashboards**
  (§6): the existing operational Grafana dashboard and this new product console.

## 4. Multi-tenancy (self-host = SaaS minus billing)

- The org model is **always present**. Self-host auto-creates one org on first
  OIDC login and sets `orgSwitcher:false`; hosted allows many.
- In-cluster: every `SandboxClaim` / `Sandbox` carries an **org owner label**.
  The BFF's `SandboxControl` seam (today an in-memory stub) gets a real
  implementation that queries the controller's CRDs filtered by that label —
  isolation enforced **at the seam**, server-side, preserving the existing
  cross-org contract (cross-org id → `not_found`, never a leak). This relies on
  the namespace/isolation model from the multitenancy isolation track (#208's
  gating dependency); it does not introduce a parallel tenancy model.
- `LogStreamer` seam → an HTTP/WebSocket proxy over the **existing** forkd→guest
  vsock exec/log transport; the BFF only authorizes (sandbox must belong to the
  caller's org).

## 5. Authentication

- **Self-host:** plugs into the operator's OIDC issuer (Dex / Keycloak / Google
  / etc.) configured via Helm values. First login binds to the auto-created org.
- **Hosted:** our hosted IdP issues the session; this is the human side of #210.
- Both produce the same session/org context the BFF expects via `WithCaller`.
  Roles/teams (`teams:true`) are on in both editions; only billing differs.

## 6. Information architecture (grounded in competitors, mapped to CRDs)

Nav follows the category convention so it reads as best-practice, but each item
maps to a primitive Mitos already has, plus Mitos-only views.

| Console section | Backed by | vs Daytona / E2B |
| --- | --- | --- |
| **Overview / Instruments** (home) | usage/metering (#211/#33) + activate-latency source | net-new proof surface (§9) |
| **Sandboxes** (live) | `Sandbox`/`SandboxClaim` via `SandboxControl` seam | parity; fork is first-class |
| → detail tabs | Overview · Logs (LogStreamer) · Terminal (existing PTY) · Filesystem (existing files API) · Metrics · Spending · **Fork tree** | Fork tree is unique |
| **Workspaces** | `Workspace`/`WorkspaceRevision` CRDs | Daytona "Volumes" are static; Mitos workspaces fork |
| **Templates** | `SandboxTemplate` CRD (ties to #209 DX epic) | = Daytona Snapshots |
| **Registries** | image-pull secrets (already in chart) | parity |
| **Secrets** | `SecretStore` provider registry (§8) | neither competitor has this |
| **API keys** | `AccountService` (#210) | parity (scoped, expiring, revocable, masked) |
| **Usage & cost** | `UsageStore` + `PriceList.Cost` (#211) | parity |
| **Billing** (hosted only) | `internal/saas/billing` (#212) + Stripe portal | parity |
| **Members & roles** | `AccountService.ListMembers` (#210) | parity |
| **Audit** | `AuditRecorder` seam | parity |

**Billing provider abstraction (decided 2026-06-22).** Billing is abstracted
behind a `billingprovider.Provider` seam — the same pattern as `SecretStore`.
Stripe is the first provider; a **Merchant of Record** (Polar / Paddle / Lemon
Squeezy) is the planned second and likely end state, because an MoR becomes the
legal seller and handles global sales-tax/VAT so Mitos does not. The console
reads billing through the provider-neutral `BillingReader`; only the webhook
(signature scheme + event names) and the portal/checkout link are
provider-specific, and both live behind the seam. The neutral `WebhookHandler`
(verify → resolve customer→org → `StatusStore.SetStatus`) and the dunning/status
core never change when the provider does.

Two dashboards ship in the chart and are different concerns:
1. **Operational** — the existing Grafana `dashboard.json` (cluster metrics, for
   the operator). Unchanged.
2. **Product console** — the new per-org user UI (this spec).

The **fork tree** is the centerpiece: it is the product differentiator and the
on-brand signature (the website's `Division` motif — magenta membrane / cyan
genome) in the view users look at most.

## 7. Brand: `@mitos/brand` package

"Fluorescence" tokens are the source of truth in `website/src/styles/tokens.css`
and `website/.interface-design/system.md`. We extract a versioned **`@mitos/brand`**
package — tokens.css + Satoshi/Geist Mono fonts + the `Division` motif + a small
set of primitives (Button, Terminal, Card) — published from the website repo and
consumed by **both** the Astro marketing site and the console SPA. One source of
truth, no drift. The console is an *interface-design* surface (dense tables and
forms), so it adapts Fluorescence to instrumentation-grade app UI — consistent
with system.md's own "not a dark SaaS dashboard" rule.

## 8. Secrets: provider abstraction + per-tenant OpenBao

Mirrors the proven paperclip model (`server/src/secrets/provider-registry.ts`,
`docs/api/secrets.md`, `docs/deploy/secrets.md`): tenant-scoped secrets behind a
pluggable provider registry, write-only after create, bound by env key, every
resolution audited.

The BFF's `SecretStore` seam becomes a **provider registry**, not a single
backend.

**Providers (org-scoped config, capability-gated):**
- **`kube` (self-host default):** materializes as **org-namespaced Kubernetes
  Secrets** in the org's pool namespace via the existing `mitos-pool-secrets`
  ClusterRole + `namespacedSecretsRBAC` path. Encryption at rest = etcd
  encryption (+ optional sealed). Zero new infra for basic self-host.
- **`openbao` (recommended external):** OpenBao (the Linux-Foundation
  open-source fork of Vault). The same driver speaks the Vault HTTP API, so it
  also covers HashiCorp Vault. OpenBao is the default-recommended external
  provider deliberately — Mitos's Pareto thesis is open-source + self-hostable,
  so the recommended secret backend must not be BSL-licensed.
- **`aws_secrets_manager`** (optional, parity with paperclip).

**Per-tenant isolation in OpenBao (operator's choice):**
- **Namespaces** (OpenBao/Vault Enterprise/HCP): one namespace per org. Hard
  isolation.
- **Path + policy scoping** (OSS default): each org gets KV-v2 path prefix
  `secret/data/orgs/{orgId}/*` plus a per-org AppRole + policy that can only read
  that prefix. The console BFF enforces org scope on top (a session for org A can
  never reference org B's path) — same server-side isolation contract as every
  other console endpoint.

**Managed modes (both providers):**
- *copy-in*: value encrypted at rest by the active provider.
- *external_reference* (strong default for OpenBao): Mitos stores only
  `{path, versionRef, fingerprint}`; the **controller resolves it at sandbox
  materialization** through the provider with binding context, writes a
  **short-lived, sandbox-scoped k8s Secret** the husk pod consumes via the
  existing `SandboxTemplate.env valueFrom secretKeyRef` path, then **GCs it on
  terminate**. One injection mechanism, any backend behind it, value never
  persisted in Mitos.

**Bindings:** secrets are not auto-env. A secret is bound into a Template /
Sandbox / Workspace env field by key (`OPENAI_API_KEY` ← stored secret, `latest`
or pinned version). Binding key ≠ stored name.

**Custody, audit, write-only:** encrypted at rest → decrypted/resolved
server-side → injected immediately before use → never returned to the UI. Every
resolution emits a non-sensitive `secret` audit event (id, version, provider,
consumer = sandbox id, outcome) to `/console/audit`. Matches paperclip's custody
chain and Mitos's existing no-secret-logging rule. Provider-config validation
rejects credential-shaped fields so provider creds never land in the store.

**Storage decision (no new CRD):** secret *metadata* + provider-vault config +
bindings live in the `internal/saas` store (like paperclip's `company_secrets`);
*values* are either org-namespaced k8s Secrets (copy-in) or resolved at runtime
(external_reference). GitOps-friendly and reuses the env-injection path already
shipped.

**Edition parity:** the provider registry is the seam; editions differ only in
which providers are configured/enabled via org-scoped config + the `secrets`
capability. Self-host defaults to `kube`, can add `openbao`; hosted runs our
managed OpenBao (per-org namespaces) or a KMS. Same binary, same registry code.

## 9. Proof surface: making the Pareto position visible and verifiable

The README's Pareto thesis: Mitos is the only runtime that is simultaneously
open-source · self-hostable · Kubernetes-native · does live N-way CoW fork of a
running microVM · warm-claim activate in the tens-of-ms class (P50 ~27 ms,
measured) · ~3 MiB marginal memory per fork via CoW · data stays on your infra.
The repo's integrity rule: numbers appear only when the in-repo harness can
regenerate them; competitor figures are labeled vendor-published / not
head-to-head. The dashboard must **prove, not boast** — which is also the brand
thesis ("only measured signal emits light").

1. **Home is an instrument panel**, not a welcome screen. It renders the org's
   own measured numbers from the existing telemetry: warm-claim activate P50/P99
   (their cluster, same activate path), **CoW density** (marginal MiB per fork
   vs naive — the same #33 metering primitive that bills also proves fork
   density), forks served / parallelism achieved.
2. **Fork tree as live Pareto evidence:** annotates each fork's private-dirty
   (unique) set against the shared parent snapshot, proving CoW page-sharing in
   the one view no competitor can render.
3. **Pareto map, honest:** a "Why Mitos" panel in onboarding / empty states
   renders the README capability matrix framed exactly as the README frames it
   (the axis is the combination; any competitor number carries the
   vendor-published / not-head-to-head label verbatim). Each axis links to live
   proof or to the reproducible bench script (`bench/husk-activate-latency.sh`,
   `cmd/bench`).
4. **"Reproduce this"** affordance on every headline metric → points at the
   in-repo harness. Integrity-as-feature.
5. **Ownership / portability badge** in the chrome: self-host shows "Running on
   your infrastructure · data never leaves"; hosted shows "Hosted by Mitos ·
   same engine, same API" plus a first-class "export / self-host this" path (and
   the E2B-migration shim). No-lock-in rendered as a feature.

**Integrity guardrail (hard spec rule):** no fabricated head-to-head numbers
anywhere in the UI; competitor figures, if shown, carry the vendor-published
label. The UI shows our measured numbers from the live system. Mirrors
`docs/saas/pricing.md`'s no-unverified-numbers rule.

Proof metrics come from the #211/#33 pipeline present in **both** editions, so
both get the live proof (capability-driven). Self-host leans on the
data-residency badge; hosted leans on portability + migration.

## 10. Gating (project rule, first-class constraint)

Per #208: public self-serve untrusted multi-tenancy does **not** switch on until
fork-correctness (ROADMAP §1, #1), failure/GC (#163), the multitenancy isolation
track, and the external security review (#194) are green. The README must keep
stating no external security review has been performed until #194 closes.

The console can be **built** in parallel but ships behind the gate. The
capability model is the enforcement mechanism:
- `signup` defaults **off** → onboarding (#215) runs in waitlist /
  design-partner mode until the gates pass.
- The console remains gated for production tenants until #194 covers it (per
  `docs/saas/console.md`).
- No edition can flip these on from the client; they are server-controlled.

## 11. Helm: one chart, two editions

New components gated by values, following the chart's existing style:

```yaml
console:
  enabled: true              # Deployment + Service + Ingress + SA/RBAC
  edition: community         # community | hosted
  oidc: { issuerURL, clientID, clientSecretRef }
  ingress: { host: console.example.com }
  signup: false              # gated; hosted enables when §10 gates pass
gateway:
  enabled: true              # API-key front door + quota
  ingress: { host: api.example.com }
billing:
  enabled: false             # hosted sets true + stripeSecretRef
secrets:
  providers:                 # registry config; kube is always available
    kube: { enabled: true }
    openbao: { enabled: false, address: "", authRef: "", perOrg: "path" }  # path | namespace
```

- **Self-host:** `helm install` with defaults → community console, OIDC, one
  org, no billing, `kube` secrets, signup off.
- **Hosted:** same chart and same images, values set `edition=hosted`,
  `billing.enabled=true`, hosted IdP, OpenBao provider, signup flipped on only
  when §10 gates pass.
- Console RBAC: read `SandboxClaims`/`Sandboxes` (org-scoped) and manage
  org-namespaced Secrets via the existing `mitos-pool-secrets` path; nothing
  privileged.

## 12. Data flow (happy path)

1. Browser → `console.example.com` → OIDC login → session cookie (org resolved;
   first self-host login binds the auto-org).
2. SPA boot → `GET /console/capabilities` → mount routes (and enforce the gate).
3. `/console/sandboxes` → BFF → `SandboxControl` → controller CRDs filtered by
   org → live list; inspect → `LogStreamer` proxy; fork → fork-tree view.
4. `/console/keys` POST → raw key shown once; `/console/usage` →
   `PriceList.Cost`; `/console/billing` → ledger (+ Stripe portal link, hosted).
5. Secrets: create/rotate via `SecretStore` provider; bind by env key; at
   sandbox materialization the controller resolves (external_reference) or
   references (copy-in) the value into the microVM env, audited.
6. Home instrument panel reads #211/#33 telemetry for the proof metrics.

## 13. Error handling

- Cross-org access → `not_found` (sandboxes/secrets by id) or 403 (membership
  verbs), per the existing BFF contract.
- Capability-off routes → never mounted client-side; BFF also returns
  `feature_disabled` defensively.
- Auth failure → redirect to OIDC login.
- Log/terminal stream drop → reconnect with backoff.
- Secret resolution failure at materialization → sandbox create fails closed
  with a typed error; never silently injects an empty value.

## 14. Testing

- BFF cross-org isolation tests already exist; extend per-endpoint for
  capability gating (billing returns `feature_disabled` in community mode) and
  for the `SecretStore` seam (cross-org secret id → `not_found`).
- Real `SandboxControl`: org-scoped cluster query tested against a fake client.
- `SecretStore`: in-memory provider tested default; `kube` and `openbao`
  providers tested behind the registry interface; binding/resolution path
  asserts write-only (value never serialized) and audit emission.
- SPA: thin renderer — component tests + a Playwright smoke against the
  `cmd/console -dev` BFF; keep light, per the repo's Go-centric philosophy.
- Helm: `helm template` golden tests for console/gateway/billing/secrets across
  `edition=community` and `edition=hosted`.
- Validation note (from #214): most UI work runs against the mock-engine control
  plane; no KVM needed.

## 15. Build order (three sub-projects; sequence in the plan)

- **Phase A — backend seams (pure Go, no JS build):** `capabilities` endpoint +
  edition config; real `SandboxControl` cluster query; `LogStreamer` proxy; OIDC
  session wiring; `SecretStore` provider registry + `kube` provider + in-memory
  default + `secret` audit type; `/console/instruments` read over #211/#33.
- **Phase B — the SPA:** `@mitos/brand` package; all console sections (§6) +
  Secrets UI (bindings + provider health/doctor) + fork-tree CoW view +
  instrument-panel home + Pareto map + ownership/portability badge, all
  capability-gated.
- **Phase C — packaging & external providers:** `Dockerfile.console` +
  `go:embed`; Helm console/gateway/billing/secrets components + golden tests;
  `openbao` provider + controller resolve-and-materialize + GC; Stripe portal +
  webhook (hosted).

The first implementation plan targets **Phase A** — it unblocks everything and
is verifiable in Go without a JS build.

## 16. Proposed new sub-issues under #208

Two ideas in this spec were net-new scope beyond the current #208 sub-issues and
have been filed:

- **Secrets management** (provider registry + OpenBao, per-tenant): filed as
  [#275](https://github.com/mitos-run/mitos/issues/275); mirrors paperclip.
- **Proof surface / instrument panel** (§9): filed as
  [#276](https://github.com/mitos-run/mitos/issues/276); depends on #211/#33.

## Open questions (resolved during brainstorming)

- UI packaging → **static SPA embedded in the Go binary**.
- Self-host auth → **OIDC, single-org default**.
- Self-host tenancy → **multi-org capable, single-org default**.
- v1 breadth → **full BFF breadth** (all current sections).
- Brand sharing → **`@mitos/brand` package**.
- Secrets storage → **provider registry, no new CRD**, default `kube`,
  recommended external **OpenBao**, per-tenant via namespace or path+policy.
