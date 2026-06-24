# Mitos Console: best-in-class dashboard (Phase B) plus enterprise layer

Date: 2026-06-24
Status: design (brainstorm output). Implementation plan is a follow-up.
Tracks: builds on the merged keystone spec
[`2026-06-22-mitos-console-design.md`](2026-06-22-mitos-console-design.md) and
the Phase A backend seams merged in PR #277. Sits under the
[#208](https://github.com/mitos-run/mitos/issues/208) hosted-SaaS epic and the
[#214](https://github.com/mitos-run/mitos/issues/214) console epic. Proposes
net-new sub-issues (see section 11).

## Summary

The keystone spec settled the architecture (one artifact, capability-driven,
SPA embedded via `go:embed`, Fluorescence brand) and Phase A shipped the Go BFF
seams (`internal/saas/console`: `SandboxControl`, `LogStreamer`, `AuditRecorder`,
`TemplateLister`, `SecretStore`, `InstrumentsSource`, plus the OIDC session and
the capabilities document). The dashboard SPA itself is still a stub
(`web/app/`: an `App` shell plus three thin views and six stub panels).

This spec designs the two things that turn that stub into the product:

1. A **best-in-class dashboard** at Linear / Vercel craft level: a real shell
   (command palette, instant navigation, optimistic UI), the full information
   architecture, and the two signature views (the instrument-panel home and the
   live fork tree).
2. An **enterprise layer**: SSO (SAML and enforced OIDC), SCIM provisioning,
   queryable audit with retention and export, data-retention policies, custom
   RBAC with resource groups, and a trust / compliance surface.

The load-bearing architectural decision is the **open-core line** (section 2):
every enterprise feature is compiled in and free; only its managed operation is a
hosted concern. Pricing and packaging are deliberately out of scope.

## Goals

- World-class developer experience: the console feels as fast and considered as
  Linear, Vercel, and PlanetScale, on the Fluorescence brand.
- Every enterprise-grade feature ships in the open-source / self-host edition
  with first-class DX. No feature is gated; the open-core line is architectural,
  not commercial (section 2).
- The architecture assumes a hosted deployment will exist and **operate** the
  managed version of those features (hosted IdP, managed audit retention,
  compliance, residency, support). Pricing, packaging, and tiering are out of
  scope for this spec (section 2).
- The payment layer is built behind an **abstract seam**: no billing provider,
  price, or packaging is baked into the design (section 2.1).
- The whole thing honors the project gates: no unverified numbers; nothing flips
  to public untrusted multi-tenancy before the gates in section 10.

## Non-goals

- Re-deciding architecture: the keystone (one artifact, capability-driven,
  `go:embed`, Go BFF) stands. This spec does not introduce Astro for the app,
  does not rewrite the BFF in Rust, and does not add a Node runtime.
- Tenant-isolation hardening itself (owned by the multitenancy isolation track,
  the #208 gating dependency).
- Engine-level metering correctness (#33) and the operational Grafana dashboard
  (already shipped, unchanged).
- Re-implementing payment UI: hosted deep-links the billing provider portal.
- Pricing, tier packaging, and any per-seat or usage price points. The design
  must support hosted billing later; choosing or naming prices is explicitly not
  this spec's job.

## 1. Stack decisions (confirmed)

- **Frontend: React + Vite SPA**, embedded in the Go console binary via
  `go:embed` (the keystone packaging). Not Astro. The dashboard is a near-100%
  interactive, stateful, authed surface (live sandbox lists, streaming logs, a
  PTY terminal, an interactive fork tree, a command palette). Astro's value is
  zero-JS content rendering and SEO, which an authed app does not use; using it
  here means shipping a SPA inside islands and adding a Node runtime, which
  breaks the single-binary property. Astro remains the marketing-site tool, and
  the `@mitos/brand` package is shared between site and console. Snappiness comes
  from architecture (single-origin serving, code-splitting, prefetch, optimistic
  UI), not from the framework.
- **Backend: the existing Go BFF** (`cmd/console`, `internal/saas/*`). Not Rust.
  The BFF is I/O-bound glue over the Kubernetes API, the billing provider, OIDC,
  and sessions, sitting on a large, tested Go spine (the #277 cross-tenant
  isolation tests). Rust buys nothing on I/O-bound work and would split the stack
  from the Go controller / forkd ecosystem. Rust stays scoped to the guest agent
  (#310 / PR #324), where binary size and cold-start latency pay off.

## 2. The open-core line (architectural principle)

**Every enterprise feature is open source with world-class DX. A hosted
deployment operates the managed version of those features; it never gates the
feature itself.** This is a technical principle about how the code is structured,
not a commercial or pricing decision. Pricing, tier packaging, and per-seat
economics are explicitly out of scope for this spec (see Non-goals).

The principle has two concrete technical consequences:

1. **Features are always present, never edition flags.** SSO, SCIM, RBAC, audit,
   retention, and export are compiled into the one binary and available in every
   deployment. The capabilities document (section 3) advertises only *who
   operates* a thing (self-managed vs hosted-managed), never *whether a feature
   exists*. No capability can switch a security feature off.
2. **Managed operation is a runtime concern, not a build concern.** The
   difference between self-host and hosted is which implementation of an operated
   seam is wired in (a self-run audit store vs a hosted retention service, an
   operator's IdP vs a hosted IdP). Same code, same features, different operator.

| Capability | OSS / self-host | Hosted (managed by us) |
| --- | --- | --- |
| SSO (SAML / OIDC), SCIM, RBAC, audit log, retention policies, audit export / SIEM, data-retention controls | All present, full DX | Same features, operated for you: hosted IdP, managed SCIM endpoint, hosted audit retention store, managed SIEM sinks |
| Compliance (SOC2 / ISO, HIPAA / BAA, DPA) | N/A by nature (contractual, not code) | Delivered: attestations, signed questionnaires, BAA / DPA |
| Data residency, VPC peering, single-tenant, dedicated capacity | Customer controls their own cluster | Guaranteed and operated |
| Uptime SLA, dedicated support | Community (GitHub / Discord) | Contractual |

This is the deliberate inverse of the "SSO tax." For an open-source,
self-hostable, K8s-native product whose whole positioning is no lock-in and
"data stays on your infra," withholding security primitives is both off-brand and
unenforceable (it is the customer's own cluster). The architecture therefore
makes every feature free and reserves only *operation* (and the contractual,
non-code things: attestations, SLA, residency guarantees) for hosting. Whether
and how hosting is priced is a later, separate decision.

### 2.1 Payment layer (abstract; no pricing baked in)

Billing already sits behind a provider-neutral seam in the codebase
(`billingprovider.Provider`, the console's `BillingReader`, and `PortalLinker`),
which the keystone spec established. This spec keeps payment fully abstract and
adds nothing concrete:

- The console reads billing only through the provider-neutral `BillingReader`
  (status, balance, ledger, caps) and links out through `PortalLinker`. It never
  imports a concrete provider.
- No price, plan, seat rate, or tier is encoded anywhere in the console or BFF.
  Quantities the system already meters (usage records, member count, active
  sandboxes) are exposed as neutral counts; turning a count into a charge is the
  provider's job, behind the seam.
- A deployment with no billing provider configured runs with `billing:false`;
  the console simply does not mount the billing view. Self-host is this case by
  default.
- The provider seam stays provider-agnostic by design so a Merchant-of-Record
  (Polar / Paddle / Lemon Squeezy) can be the implementation as easily as Stripe,
  with only the webhook signature scheme and the portal link living behind it
  (the keystone billing-abstraction decision, unchanged).

The net effect: the hosted deployment *can* bill per seat (or any other way)
later by wiring a provider, but this spec commits to no pricing model and bakes
none into the code.

## 3. Capabilities model (minimal change)

The enterprise security features are **always present**, never edition flags.
The capabilities document keeps differing only on managed-operation flavors and
the existing edition fields. New, server-controlled, never client-set:

```
GET /console/capabilities  ->
{
  "edition":      "community" | "hosted",
  "billing":      false | true,
  "signup":       false | true,
  "teams":        true,
  "sso":          { "oidc": true, "saml": true },   // features, always on
  "scim":         true,                              // endpoint always present
  "idp":          "operator" | "hosted",             // who runs the IdP
  "auditRetention": "local" | "managed",             // who runs the store
  "auditSinks":   ["s3","webhook","splunk","datadog"],
  "residency":    "self" | "guaranteed",
  "compliance":   false | true,                      // artifacts delivered
  "orgSwitcher":  false | true,
  "secrets":      { "providers": ["kube"] | ["kube","openbao", ...] },
  "proof":        true,
  "ownership":    "self-hosted" | "hosted"
}
```

`edition`, `billing`, `signup`, `orgSwitcher`, `idp`, `auditRetention`,
`residency`, and `compliance` are the only fields that differ by deployment, and
they describe who operates a thing, not whether a feature exists. This preserves
the keystone's "server decides, client renders" discipline and the section 10
gate-enforcement mechanism.

## 4. Dashboard craft (Linear / Vercel grade)

### 4.1 Design system

Adapt the existing `@mitos/brand` Fluorescence tokens
(`web/packages/brand/src/tokens.css`) to instrumentation-grade app UI: elevation
by the `--field` lightness ladder (never shadow), color as meaning only (magenta
= division / fork, cyan = genome / parent, green = live, amber = warning),
Satoshi / Geist Mono, the 8px spacing scale, and the `--ease` / `--dur` motion
tokens. This stays inside the brand book's own "not a dark SaaS dashboard" rule.
The interface-design skill drives this work in phase B0.

Primitives to build (in `@mitos/brand` where shared, else in the app):
`AppShell`, `NavGroup`, `CommandPalette`, a sortable / filterable / virtualized
`DataTable`, `DetailDrawer`, `Tabs`, `EmptyState`, `Toast`, `Skeleton`, form
primitives, `StatTile` (instrument), `CopyOnce` (raw key / token, shown exactly
once), and the existing `Terminal` and `Division` from brand.

### 4.2 Interaction model (the source of "snappy")

- Client-side router (TanStack Router or React Router) with route-level
  **prefetch on hover / focus** and per-route code-splitting.
- **Optimistic mutations** with rollback: create-key, revoke, terminate, invite,
  and secret bind render instantly and reconcile against the BFF response.
- Data layer: a typed client (extend `web/app/src/api.ts`) over TanStack Query
  for stale-while-revalidate, background refresh, and request dedup. Live data
  (sandbox list, logs) over polling or the `LogStreamer` WebSocket.
- **Command palette (Cmd-K):** fuzzy navigation to any view, org, or sandbox,
  plus actions (fork a sandbox, create a key, invite a member, open docs).
- Perceived performance: skeletons not spinners, single-origin `go:embed`
  serving (no cross-origin round trips), prefetch.

### 4.3 Information architecture (grouped nav)

| Group | Views |
| --- | --- |
| **Run** | Instruments (home cockpit); Sandboxes (list + detail tabs: Overview, Logs, Terminal, Filesystem, Metrics, Spending, Fork tree) |
| **Build** | Workspaces; Templates; Secrets; API keys |
| **Govern** | Members & roles; Audit; Data & retention; SSO; Trust & compliance; Usage & cost; Billing (hosted) |
| **Settings** | Organization; Profile; Theme |

Each view maps to an existing BFF endpoint or a seam this spec extends (section
6, section 5). Every view ships a crafted empty state that teaches and links to
its first action, never a blank table.

### 4.4 The two hero moments

1. **Instruments home (cockpit).** Reads `/console/instruments` (#211 / #33).
   `StatTile`s for warm-claim activate P50 / P99 (their cluster, same activate
   path), CoW density (marginal MiB per fork), and forks served / parallelism
   achieved. Every headline metric carries a **"Reproduce this"** popover that
   names the in-repo bench (`bench/husk-activate-latency.sh`, `cmd/bench`).
   Integrity as a feature. Honest empty state before any data exists.
2. **Live fork tree.** The brand `Division` motif rendered as an interactive CoW
   tree (Canvas or D3): the parent snapshot at the root (cyan genome), forks
   radiating (magenta membrane), each node sized and annotated by its
   private-dirty (unique) page set against the shared parent. Hover for per-fork
   stats; click to jump to that sandbox. The one view no competitor can render.
   Renders from the sandbox / claim records plus the #33 CoW metering primitive.

### 4.5 Onboarding and empty states

A crafted first-run flow (connect / SSO, create or fork a first sandbox, watch
it appear in the cockpit). The honest "Why Mitos" Pareto panel from the keystone
spec section 9 lives in empty states, framed exactly as the README frames it
(the axis is the combination; any competitor figure carries the vendor-published,
not-head-to-head label verbatim).

## 5. Enterprise layer (all OSS, hosted operates)

### 5.1 SSO: SAML, enforced OIDC, and SCIM

Today `internal/saas/oidcauth` issues a session from an OIDC login and
`WithCaller` attaches the account and org; there is no SAML, no org-enforced SSO,
and no provisioning.

- **OIDC (exists, hardened):** keep `oidcauth`; add an org-level **enforce-SSO**
  setting so a member of an SSO-enforced org must authenticate through the org's
  IdP. Self-host configures the issuer through Helm values (keystone section 5).
- **Federation broker (non-SAML): Dex.** For LDAP, GitHub, Google, and generic
  OIDC, the recommended self-host pattern is to front auth with Dex (the CNCF
  OIDC broker), which exposes a single OIDC interface to the console. This keeps
  `oidcauth` the only app-side integration for that whole family and is the
  standard K8s-native pattern (ArgoCD, Harbor). Dex is optional: an operator can
  point `oidcauth` straight at any OIDC issuer (Keycloak, Okta, Google) instead.
- **SAML 2.0 (direct, not via the broker):** a new `internal/saas/saml`
  provider that issues the same session contract as `oidcauth` (SP-initiated and
  IdP-initiated, an ACS endpoint, and metadata exchange), mapping the assertion
  to an account and org membership. **Library: `crewjam/saml`** (the de-facto
  standard Go SAML library; its `samlsp` middleware handles the ACS endpoint,
  metadata, and session plumbing, so the security-critical XML/assertion code is
  the maintained library's, not ours). `russellhaering/gosaml2` (SP-only, pure-Go
  XML-dsig, actively maintained) is the lean alternative if we choose to own the
  ACS/session glue. SAML deliberately does **not** route through Dex: Dex's own
  SAML connector is flagged unmaintained and prone to auth bypass, so SAML uses
  the dedicated library directly. Every SAML library carries CVE history (SAML is
  inherently footgun-prone), which is exactly why we lean on maintained
  middleware rather than parsing assertions ourselves, and why the ACS path is a
  named-human-reviewer surface (section 9).
- **SCIM 2.0 provisioning:** a new `/scim/v2/Users` and `/scim/v2/Groups`
  endpoint (RFC 7644) authenticated by a per-org provisioning bearer token
  (write-only after creation), mapping SCIM Users to accounts and memberships and
  SCIM Groups to roles or resource groups, with just-in-time deprovisioning on
  delete.
- **DX (the differentiator):** an **SSO setup wizard** view: paste the IdP
  metadata URL or upload the XML; the console shows the SP entityID / ACS URL to
  paste back; a **Test login** button runs a real round trip and renders the
  decoded assertion attributes; a **connection doctor** diagnoses clock skew,
  certificate expiry, and attribute mapping. Setup that does not require a
  support ticket is the whole point.
- Capability: `sso.{oidc,saml}` and `scim` are always true (features present);
  `idp: hosted` only means we run the IdP, not that SSO is unlocked.

### 5.2 Audit: query, retention, export

Today `AuditEvent{OrgID, ActorID, Action, Target, Detail, At}` lands in an
in-memory reverse-chronological `MemAuditLog`, and only key create / revoke and
sandbox terminate are recorded.

- **Coverage:** emit an audit event on every state-changing verb: the existing
  key and terminate events, plus member add / remove / role-change, secret create
  / delete / bind / resolve (the keystone's `secret` resolution audit flows
  here), SSO and retention-policy changes, and sandbox create / fork. No secret
  value ever enters an event (the existing rule).
- **Query:** grow `GET /console/audit` with filters (actor, action, target, time
  range) and cursor pagination. The UI is a searchable, filterable table with a
  detail drawer.
- **Retention policy:** a per-org `AuditRetention{days}` swept by the GC path
  (ties to #163). OSS ships a sensible rolling default; it is configurable.
- **Export / SIEM:** an `AuditSink` registry (the same seam pattern as the
  `SecretStore` provider registry) with sinks for S3, webhook, Splunk HEC, and
  Datadog, stream-on-write plus an NDJSON backfill download. All OSS; hosted
  operates the retention store and the managed sinks.
- **Storage:** `AuditRecorder` gains a durable implementation in the
  `internal/saas` store; the interface is unchanged so the existing cross-org
  isolation tests stay green.

### 5.3 Data-retention policies

Per-org declarative retention for terminated-sandbox metadata, sandbox logs,
usage records, and audit records, enforced by the controller GC path (#163) plus
the BFF sweeper. A "Data & retention" view exposes per-resource windows, a
"what gets deleted when" preview, and a legal-hold toggle. This is the privacy /
DPA retention canon rendered as product.

### 5.4 RBAC and resource groups

Today `Role = owner | member` (the model comment notes "richer RBAC is a
follow-up"). Keep `owner` and `member` as built-in roles so existing tests and
behavior hold, and add additively:

- **Custom roles:** a named role maps to a permission set over the console verbs
  (keys, secrets, sandboxes, members, billing, audit, settings).
- **Resource groups:** a sandbox, template, or secret can belong to a group; a
  membership grants a role *within* a group. This composes with the org-scoping
  already enforced at every seam (a session for org A still never sees org B).
- Surfaced in a real Members & roles UI: invite, assign role, build a custom role
  on a permission matrix, manage group membership.

### 5.5 Trust and compliance surface

A "Trust & compliance" view:

- **OSS / self-host:** renders the threat-model status
  (`docs/threat-model.md`), the **no-external-security-review honesty banner**
  (the #194 gating rule; the banner stays until #194 closes), the self-host
  data-residency posture ("data never leaves your infrastructure"), and a
  downloadable self-host security checklist.
- **Hosted:** surfaces the SOC2 Type II / ISO 27001 report request, the BAA /
  DPA, the sub-processor list, and the status page / SLA. These are the
  delivered (operated) artifacts, per the open-core line.

## 6. BFF surface delta (over the merged seams)

| Surface | Change |
| --- | --- |
| `GET /console/capabilities` | add the section 3 fields |
| `GET /console/audit` | add filter + cursor pagination |
| `AuditRecorder` | durable impl; retention sweep; `AuditSink` registry |
| `AuditEvent` emission | extend to all state-changing verbs |
| `internal/saas/saml` | new SAML session provider |
| `/scim/v2/*` | new SCIM 2.0 provisioning endpoint |
| `AccountService` roles | custom roles + resource groups (additive) |
| retention | per-org policy store + GC integration (#163) |
| `/console/instruments` | back the cockpit StatTiles (exists) |

Every new or changed endpoint keeps the org-scoped isolation contract and gets a
cross-org isolation test (section 8).

## 7. Build sequencing (each phase is one PR)

- **B0 (design system + shell):** `@mitos/brand` consumption, `AppShell`,
  grouped nav, client-side routing with prefetch, the Cmd-K command palette, the
  data layer (typed client + query cache), and the empty-state / onboarding
  primitives. Driven by the interface-design skill.
- **B1 (hero views):** the Instruments cockpit and the live fork tree.
- **B2 (core views):** Sandboxes (list + all detail tabs), Secrets, API keys,
  Usage & cost, Members, Audit (query UI), Templates, Workspaces, Billing
  (hosted).
- **B3 (enterprise layer):** SSO setup wizard (SAML + enforced OIDC) and the
  SCIM endpoint; audit retention + export sinks; data-retention policies; custom
  RBAC + resource groups; the Trust & compliance view.

Most of this runs against the mock engine and the existing BFF; no KVM is
needed (the keystone validation note).

## 8. Testing

- BFF cross-org isolation tests already exist; extend per new endpoint
  (audit filters, SCIM, retention, custom-role checks) so a session for org A can
  never read or write org B's data through any new surface.
- SAML: assertion-to-session mapping, signature verification, clock-skew
  handling, and IdP-initiated flow, all against a fake IdP.
- SCIM: User and Group create / update / delete mapping to memberships and roles,
  bearer-token auth, and cross-org rejection.
- Audit: retention sweep, sink delivery (fake sinks), filter + pagination, and
  the no-secret-in-event invariant.
- RBAC: custom-role permission checks and resource-group scoping, with the
  built-in `owner` / `member` behavior unchanged.
- SPA: component tests for the primitives, plus a Playwright smoke against
  `cmd/console -dev`; kept light, per the repo's Go-centric philosophy.
- Helm: `helm template` golden tests for the new capability fields across
  `edition=community` and `edition=hosted`.

## 9. Security and threat-model deltas

New surfaces move the threat model and must be updated in the same PR
(`docs/threat-model.md`, per CLAUDE.md):

- SAML ACS endpoint (assertion replay, signature wrapping, audience / recipient
  validation, clock skew).
- SCIM endpoint (provisioning-token custody, write-only after create, cross-org
  rejection, deprovisioning correctness).
- Audit sinks (egress of audit data to third-party SIEMs; sink credential
  custody follows the no-secret-logging rule; only audit metadata leaves).
- Retention / legal hold (deletion correctness and the legal-hold override).

SSO, SCIM, and audit-export code paths are security-sensitive and require a named
human reviewer before merge, alongside the existing sensitive paths
(`internal/fork`, `internal/firecracker`, `internal/daemon`, `guest/agent`).

## 10. Gating (project rule, unchanged)

Per #208, public self-serve untrusted multi-tenancy does not switch on until
fork-correctness (#1), failure / GC (#163), the multitenancy isolation track, and
the external security review (#194) are green. The README keeps stating that no
external security review has been performed until #194 closes, and the Trust view
renders that banner. The console is built in parallel but ships behind the gate;
`signup` defaults off (waitlist mode). No edition can flip these from the client;
they are server-controlled capability fields.

## 11. Proposed new sub-issues under #208

These are proposals only; they are not filed yet.

- **Enterprise SSO: SAML + enforced OIDC + SCIM provisioning** (with the setup
  wizard and connection doctor DX).
- **Audit: queryable log + retention policy + export / SIEM sinks.**
- **Data-retention policies** (per-org, GC-enforced; ties to #163).
- **Custom RBAC + resource groups** (additive over `owner` / `member`).
- **Console dashboard SPA (Phase B): shell, hero views, core views** (B0 to B2).

The Trust & compliance view attaches to the existing #208 and #276 (proof
surface) issues rather than a new one.

## 12. Open questions (resolved during brainstorming)

- Scope: design the dashboard and the enterprise layer **together** so the IA
  carries the enterprise surfaces from day one.
- Open-core line: **every enterprise feature is OSS with world-class DX; a
  hosted deployment operates the managed version, never the unlock**. This is an
  architectural principle; pricing and tiering are out of scope (section 2).
- Payment layer: kept fully **abstract** behind the existing provider seam, with
  no provider, price, or packaging baked in (section 2.1).
- Design feel: **Linear / Vercel-grade product polish** (command palette,
  instant navigation, optimistic UI, crafted empty states), with the instrument
  cockpit and live fork tree as the hero moments.
- Frontend: **React + Vite SPA**, not Astro (section 1).
- Backend: **the existing Go BFF**, not Rust; Rust stays in the guest agent
  (section 1).
