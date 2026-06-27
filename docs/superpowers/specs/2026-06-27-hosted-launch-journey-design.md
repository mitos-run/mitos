# Hosted launch: end-to-end journey and pricing design

Date: 2026-06-27
Status: design (brainstorm output). Implementation plans are follow-ups, one per workstream (section 12).
Tracks: the hosted-SaaS epic (#208) and the Run-with-Mitos line (#340, gated public path #341). Sits above the existing SaaS spine (`internal/saas/*`, `cmd/gateway`, `cmd/console`) and the website (`mitos.run`).

## Summary

The hosted offering already has a tested backend spine: accounts/keys, per-org tenancy, CoW-aware metering, usage aggregation, Stripe-seam billing with credit ledger / spend caps / dunning, an org-scoped console BFF, and a substantial console SPA. The marketing site is strong and on-brand ("Fluorescence"). What is missing is the connective tissue that turns a stranger into an activated, paying user: the journey from search intent through onboarding to a first "it worked" moment and an honest billing relationship. Today every marketing CTA points at `/docs/quickstart`; there is no hosted signup anywhere.

This spec designs that journey end to end as one system, under one principle, and decomposes the build into six workstreams. The goal is not a new SaaS; it is a world-class experience laid over machinery that mostly exists.

North star: the Apple for agents. Strong brand, dead simple to use, loved end to end. The operating tension we hold everywhere: broad intent coverage without surface complexity. Simple surface, depth one click down.

## Principle: one machine, many on-ramps

The aha is intent-dependent. A reinforcement-learning team, a code-interpreter integrator, and someone deploying openclaw each have a different moment where it clicks. We do not build N products for this. We build one shared spine and many on-ramps:

- One spine: signup, key, console, billing, guardrails.
- Many on-ramps: each use-case page seeds context (which template, which snippet, which guided first-run) and routes to a tailored first-success that is the same mechanism parameterized differently.

The use-case page is not SEO bait bolted onto a generic funnel. It preloads the journey so the aha is already use-case-shaped by the time the user lands in the console.

## Research grounding (verified, 2026-06)

Decisions below are grounded in a verified deep-research pass (search-intent + competitive + coding-agent integration surface). Load-bearing findings:

- RL / parallel-rollout intent is wide-open white space. Only one competitor (Beam) holds a dedicated "sandbox for RL" page, while academic demand (DeltaBox millisecond checkpoint/rollback, NVIDIA ProRL Agent rollout-as-a-service, Tree-GRPO) is surging. Live-fork is the exact primitive these workloads need. State-management overhead is 47-77% of agent trajectory time on coupled-filesystem baselines, cut to 3-6% with millisecond checkpoint/rollback.
- "E2B alternative" is high commercial intent, decided on four buyer criteria: isolation model, deployment/data control (BYOC), scale, lifecycle/state.
- Claude Code open-sourced its sandbox-runtime (`@anthropic-ai/sandbox-runtime`) and explicitly invites adoption. It is OS-level (bubblewrap / Seatbelt), not a VM. That is the precise gap a Firecracker microVM fills beneath it.
- OpenAI Codex cloud is a container + `universal` polyglot base image with a setup-script / `CODEX_ENV_*` config contract.
- A "universal agent API over HTTP" pattern is consolidating (rivet-dev/sandbox-agent, coder/agentapi) that normalizes Claude Code, Codex, opencode, Cursor, Amp behind one HTTP/SSE interface. MCP-native is becoming table-stakes for sandboxes.
- Two landmines to avoid in copy: the fabricated "79% use agents / 5% solved sandboxing" stat (refuted), and "sub-second boot" as a blanket claim. Both violate the no-unverified-claims rule. We stay precise: quote Firecracker boot and our own reproducible fork latency only.

Live-fork itself is becoming table-stakes (Daytona, Runloop, Morph, Freestyle all ship some snapshot/branch/fork). We differentiate on fork latency at scale, Firecracker isolation, open source, and the experience, never on "fork exists."

## Decisions locked in this brainstorm

- Launch shape: open no-card self-serve is the target UX, built fully, behind the existing `ModeWaitlist` / `ModeOpen` flag and the gate workstream. The goal is full implementation including resolving the multitenancy gate (section 11), not shipping dark indefinitely.
- Signup credit: $5 (not $100). "Try it," not "run a month." Raises the stakes on first-success.
- Launch intents: rollouts, code-execution, switch-from-E2B/Daytona/Modal, coding-agent runtime, evals, and run-my-repo/apps (openclaw). openclaw/packaged is the fast-follow gated on #341.
- Homepage hero unchanged ("fork a microVM into an agent swarm"). Use-case pages live beneath it; rollouts is the top use-case page, not a new homepage hero.
- Navigation: one restrained "Use cases" panel (By workload / By integration / Run an app), not a sprawling mega menu. Protects the minimal brand.
- Coding-agent integration: full investment at launch (MCP-native, universal HTTP adapter compatibility, Codex universal-image contract, plug-under-Claude-Code), accepting a later launch date for the larger scope.
- Pricing: three shapes chosen by angle, never one generic table. Packaged (per-app subscription), Box (reserved flat), Metered (PAYG). metered + box at launch; packaged with the openclaw fast-follow.

## The spine (every intent flows through this)

Front door -> intent-matched use-case page -> no-card signup ($5 credit) -> tailored first-success (the aha) -> see it work + see the cost (trust loop) -> expand (pick pricing shape / add card / invite team / go to prod).

No step is a dead end. Each step carries the use-case context forward so the next step is pre-shaped.

## The core map (intent -> page -> aha -> pricing)

| Intent | Real search language | Page | First-success aha | Pricing |
|---|---|---|---|---|
| RL / parallel rollouts (lead use-case) | "sandbox for RL rollouts", "best-of-N", "tree search / MCTS", "parallel rollouts" | `/use-cases/rollouts` | fork one warm env into N, live fork tree + per-fork cost | metered + box |
| Run code from my app (broad) | "code execution sandbox for AI agents", "run untrusted / LLM code", "code interpreter API" | `/use-cases/code-execution` | their snippet runs in a real microVM, output streams back | metered |
| Switch from E2B/Daytona/Modal (high CI) | "E2B alternative", "Daytona vs E2B", "open-source E2B alternative" | `/alternatives` + `/compare/*` (sharpen) | existing code runs with a 2-line change | metered |
| Coding-agent runtime (magnet) | "run Claude Code / Codex in a sandbox", "BYO runtime", "background agent" | `/use-cases/coding-agents` + `/integrations` | point Claude Code / Codex at a Mitos box, runs isolated; MCP one-click | box or metered |
| Multi-agent evals | "agent evals at scale" | `/use-cases/evals` | each agent its own computer, run the matrix | metered / box |
| Run my repo / apps (openclaw) | "deploy agent app", "run with mitos" | `/run` + `/apps/openclaw` | click badge -> live URL at `you.openclaw.mitos.run` | packaged (subscription) |

## Workstream 1: Marketing surface (website)

Goal: turn search intent into an activated signup, on-brand, simple on the surface.

### Navigation
Keep the four-link restraint. Add one "Use cases" trigger (label leaning "Use cases" over "Solutions"; see open questions) that opens a single organized panel:

- By workload: RL rollouts, code execution, multi-agent evals, untrusted code.
- By integration: Claude Code, Codex, opencode, MCP.
- Run an app: openclaw (and the awesome-mitos front door later).

Constraints: one panel, instrument-grade, roughly nine links max, each with a short descriptor. No icons-wall, no featured-card clutter, no second accent color. Honors the brand anti-slop rules (no three identical cards, no mega-menu sprawl). Simple trigger, organized depth. Keyboard and reduced-motion accessible like the existing nav.

### Use-case page template (data-driven)
One template, many pages, driven by a typed data file the way `src/data/competitors.ts` drives `/compare/[slug]`. Proposed `src/data/usecases.ts` with a `UseCase` interface. Each page renders:

1. Hero: intent-matched concrete promise (verb + object/number), one real number, imperative CTA.
2. The problem, in the user's own words and search language (the pain, named honestly).
3. The mechanism: fork, shown with the Division component. One biology word, where true.
4. A copy-paste snippet that is a preview of the aha (the exact code the console first-run will run).
5. Proof: a reproducible benchmark number (from `bench/`), never an unverified claim.
6. A comparison sliver vs the one obvious alternative for this intent (reuse `competitors.ts` framing).
7. The right pricing teaser for this intent (metered calculator, box, or packaged price).
8. CTA into signup pre-seeded with this use case's template id (carried as a query param the console reads on first login).

SEO/AEO: titles and copy use the verified keyword phrasings; structured data per the schema discipline; pages are answer-engine-citable (clear claims, sources, numbers). The rollouts page is the priority build (uncontested intent).

### Integrations page
`/integrations` as a developer magnet (workstream 5 supplies the runtime). Per-tool sections (Claude Code, Codex, opencode, Cursor, MCP) with the exact wiring and a runnable example. Honest about what is native vs adapter-based.

### Messaging discipline
Clear and simple per "Apple for agents," but always in brand voice: prove don't persuade, falsifiable claims, no slop lexicon, no em/en dashes, headlines are concrete promises. Forbidden: the refuted demand-gap stat, blanket "sub-second boot." Approved precise numbers only.

## Workstream 2: Onboarding funnel (UI + auth + wiring)

Goal: signup to first successful run in minutes, no card, one SDK package, a snippet that works first try (the most-cited reason developers choose a sandbox provider).

- Auth: email magic-link plus GitHub and Google via the existing OIDC handlers (`internal/saas/oidcauth`), wired to `cmd/console` `/auth/*` with a session cookie. This is the documented seam to make real.
- Provisioning: reuse the onboarding service (signup -> verify -> personal org -> $5 credit -> first key shown once). Change `DefaultSignupCredit` to $5.
- Seeded context: the signup URL carries the use-case template id; the console reads it and shapes the first-run.
- Durable stores: replace in-memory `PendingStore` / accounts `Store` with Postgres behind the existing interfaces.
- Email: real provider behind the `EmailSender` seam (SES/SendGrid/Resend, decision in the plan). Raw tokens never logged.
- Guardrails the happy path never feels (also the #341 abuse posture): hard spend cap default-on, per-org rate caps, idle TTL, default-deny egress.
- Metric: median time-to-first-sandbox is the headline funnel number (already instrumented via `EventRecorder`); wire a real analytics sink and the live dashboard.

## Workstream 3: Console first-run (the aha surface)

Goal: the tailored first-success per intent, and the trust loop, in the existing capability-driven SPA.

- Guided first-run, shaped by the seeded template id. Default per intent:
  - Rollouts / swarm: the signature moment. Spawn a warm sandbox, `fork(8)`, watch the live fork tree light up with per-fork memory and cost ticking. This is the Division made real, in-product, and is uncopyable. Builds on the console "proof / instrument" panel already in the console design spec.
  - Code-execution: an in-console runnable snippet against the user's real key, output streams back.
  - Coding-agent: a "connect Claude Code / Codex" step that emits the exact config to point the agent at a Mitos box (workstream 5).
- Trust loop: real-time cost is visible everywhere (the live fork tree, the usage view). Seeing the cost while it works is the trust-builder, especially with only $5 of credit.
- One artifact: the same console image and SPA bundle serve self-host and hosted, differing only by the capabilities document. No edition build fork.
- Wire the live-sandbox and log-streaming seams (`SandboxControl`, `LogStreamer`) to the real control plane (depends on workstream 6 isolation).

## Workstream 4: Billing and pricing UX (three shapes)

Goal: the right way to pay for the angle, simple on the surface, honest underneath.

### Pricing page information architecture
Not one generic table. The page opens with "How do you want to pay?" and three clean paths:

- Pay as you go (metered): the existing live calculator. Per-second decoupled vCPU and RAM, storage GiB-hours, metered egress, GPU. For bursty swarms, code-from-app, migration-from-E2B.
- Reserve a box (box): a fixed vCPU/RAM pool at a flat price, warm capacity reserved, no bill surprise. For steady rollout/eval teams. New.
- Buy an app (packaged): a per-app subscription (openclaw at a flat monthly price, always-on, auto-updated; the buyer never sees vCPU). Fast-follow with #341. New.

Each path is simple; depth (the full rate card, the cap controls) is one click down.

### Billing model extension
- Metered exists end to end (CoW-aware metering -> usage records -> Stripe metered push, idempotent).
- Box and packaged are Stripe subscriptions layered on the same seam, plus a metered overage line for usage beyond the bundle. Reuse the credit ledger, spend caps, dunning, and the single rate table (`billing.FromPriceList`) so display cost and billed cost never drift.
- $5 signup credit; hard spend cap default-on (a runaway swarm cannot make an unbounded bill); top-up ladder; dunning state machine; Stripe Customer Portal deep-link for invoices and payment methods.
- Production wiring: real Stripe SDK adapter and real webhook signature verification behind the existing interfaces, run with test-mode then live keys. Keys and signing secrets are secrets: never logged, never in errors. This passes the pricing production gate review.

## Workstream 5: Coding-agent integration (full investment)

Goal: be the computer behind the coding agents, fully, at launch.

- Plug-under-Claude-Code: be the Firecracker hardware-isolation layer beneath Claude Code's OS-level open sandbox-runtime, which invites exactly this. Document and ship the wiring.
- Universal HTTP adapter compatibility: ship a control surface compatible with the consolidating universal agent API pattern (rivet-dev/sandbox-agent, coder/agentapi) so Claude Code, Codex, opencode, Cursor, Amp drop in over one HTTP/SSE interface.
- Codex universal-image contract: a polyglot base image plus the setup-script / `CODEX_ENV_*` configuration contract so existing Codex / devcontainer workloads run unmodified.
- MCP-native: an MCP server available in every sandbox (table-stakes per the research), plus one-click "enable Claude Code" on a box.
- The `/integrations` page (workstream 1) is the front door to all of this with runnable examples.

Honesty rule: state precisely what is native versus adapter-based; do not overclaim parity.

## Workstream 6: The gate (multitenancy isolation + #341)

Goal: actually resolve the gates so open self-serve and the public/openclaw path can go live, with care, because this is what protects users.

- Isolation hardening: wire `SandboxControl` to real org-filtered CRD queries; enforce per-org namespace isolation (`mitos-org-<id>`) and network default-deny egress; preserve the cross-org contract (cross-org id -> not_found, never a leak).
- Verification: a cross-tenant chaos / isolation test suite that proves isolation under failure (node loss, slow etcd, capacity exhaustion), plus residual garbage collection (orphan-VM sweeps). The "fast self-serve UX" and "hard isolation" claims are measured, not asserted.
- #341 for the public / openclaw path: AUP / ToS, abuse detection plus terminate-and-quarantine kill-switch (kill-switch exists), the secret-consent and egress-disclosure screen from the Run-with-Mitos design, DMCA designated agent and the counsel-led legal items.
- External security review: built and verified isolation first, designed for the smallest possible review surface. Commissioning the review (internal vs firm) is a business decision; this workstream makes it cheap and fast.

Flipping `ModeOpen` and enabling the openclaw public path are gated on this workstream passing. This is the highest-care, highest-risk workstream and is sequenced accordingly.

## UX as DNA (cross-cutting deliverable)

Per the directive that experience is core DNA (the Apple/Google/Amazon bar), add an explicit, enforceable UX principle to the canon so every future surface inherits it:

- Brand book (`website/docs/brand/brand-book.md`) and the interface-design `system.md`: a short "journey has no dead ends; simple surface, depth one click down; the aha is intent-shaped" principle, with the anti-patterns (mega-menu sprawl, generic pricing table, untailored onboarding).
- `mitos/CLAUDE.md` and `website` equivalent: an operating principle line so the discipline is applied in code review and `/audit`, `/critique`.

This is done as part of workstream 1 so it is in force before the surfaces are built.

## Sequencing

Per the chosen approach: design first (this spec), then build in slices with review gates. Recommended order:

1. Workstream 1 (marketing surface) and the UX-DNA canon update, in parallel with the start of 6.
2. Workstream 2 (onboarding + auth) and Workstream 6 (the gate) proceed together; 6 is the long pole.
3. Workstream 3 (console first-run) once 2 and the isolation seams from 6 are ready.
4. Workstream 4 (billing: metered live, box added) once 2 and 3 exist.
5. Workstream 5 (coding-agent integration) in parallel from early, since it is largely runtime and docs.
6. Fast-follow: packaged pricing + openclaw public path, when #341 in workstream 6 clears.

Each workstream gets its own spec -> implementation plan -> build -> review checkpoint. This document is the master; the per-workstream specs are children.

## Success metrics

- Median time-to-first-sandbox (signup -> first_exec), the headline activation number.
- Funnel conversion per step (signup_started -> verified -> key_issued -> first_sandbox_created -> first_exec).
- Free-to-paid conversion (credit exhaustion -> card added -> first paid usage).
- Intent-page to signup conversion, per use-case page.
- Honesty guardrail: zero unverified public numbers (every number reproducible from `bench/`).

## Non-goals

- Re-implementing payment UI (deep-link Stripe Customer Portal).
- Engine-level metering correctness (owned by #33, done).
- A new tenancy model (workstream 6 hardens the existing per-org namespace model, it does not replace it).
- Changing the homepage hero or the Fluorescence brand direction.

## Open questions

- Nav label: "Use cases" vs "Solutions" (lean "Use cases", developer-honest).
- Email provider choice (SES vs SendGrid vs Resend), decided in the workstream 2 plan.
- Box pricing granularity (fixed sizes vs configurable), decided in the workstream 4 plan.
- Whether the awesome-mitos curated front door ships with the openclaw fast-follow or later.
- Deployment path specifics via the paperclip / mono repo on this machine (to map at build/ship time).

## Risks

- Workstream 6 (the gate) is the long pole and the highest-risk; underestimating it risks an unsafe launch. It is sequenced first and given the most care.
- Full coding-agent integration at launch widens scope and pushes the date; accepted deliberately for the developer-magnet payoff.
- $5 credit raises first-success pressure; if activation is weak the funnel leaks. Mitigated by the tailored first-run and the real-time trust loop.
