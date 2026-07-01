# Hosted journey completion: Get Started to first-run aha to paid, gated by allowlist

Date: 2026-07-01
Status: design (brainstorm output, approved). Implementation plan is the follow-up.
Parent: `docs/superpowers/specs/2026-06-27-hosted-launch-journey-design.md` (the master). This spec completes the connective tissue across that master's WS1 (marketing CTA), WS2 (onboarding + auth + abuse), WS3 (console first-run), and WS4 (billing UX), and adds the allowlist gate.
Prior WIP being resumed: `hosted-launch-ws4` (carries all of `hosted-launch-ws3` plus the ledger fix), `feat/console-ux-polish`, `fix/215-onboarding-verify-idempotent`.

## Summary

The hosted spine already exists and mostly works: signup/verify/provision, OIDC, durable Postgres sessions and usage store, a credit ledger with a prepaid top-up primitive, both a Paddle and a Stripe billing adapter (Paddle selected when its keys are present), a capability-driven console SPA with nearly every dashboard view, and a beautiful live fork-tree and proof-instruments home. A prior session began the journey work and was terminated mid-flight. This spec resumes that work and lifts it to a world-class bar.

The goal is one loved end-to-end journey: a live Get Started button, an allowlisted signup that confirms email and resists abuse, an intent-shaped first-run that ends in a real "it worked" moment, credits and a way to buy more, and a dashboard that is a pleasure to use. The whole thing ships behind the signup flag and an allowlist. No public flip, so the isolation gate (master WS6) is not a blocker for shipping this gated.

## What already exists (verified against origin/main @ 1.10.0)

- Website CTAs read one constant, `website/src/data/site.mjs` `SIGNUP_BASE` (today `/docs/quickstart`); `signupUrl(slug)` appends `?uc=<slug>`. Use-case page template and the `rollouts` data entry exist; the use-cases nav is behind `SHOW_USE_CASES = false`.
- Onboarding: `internal/saas/onboarding` SignUp -> email verify (hashed token, 24h TTL, idempotent) -> provision account + personal org + signup credit + first key shown once. `ModeWaitlist` (records entry, no provisioning) vs `ModeOpen`. Gated by `MITOS_CONSOLE_SIGNUP`; when on, mode is `ModeOpen`. Signup credit override `MITOS_CONSOLE_SIGNUP_CREDIT_CENTS`.
- Auth: `internal/saas/oidcauth` (login/callback/logout, GitHub/Google connector hints), `LoginManager` find-or-create by verified email, durable `PgSessionStore` (hash only). `cmd/frontdoor` routes marketing vs console and resolves sessions.
- Billing: `internal/saas/billing` credit ledger (`KindSignupCredit`, `KindTopUp`, `KindUsageDrawdown`), `TopUp()`, `TopUpLadder()`, dunning state machine; `internal/saas/billingprovider` neutral `Provider` seam (`Name`, `VerifyWebhook`, `PortalURL`) with real `paddle/` and `stripe/` adapters; `cmd/console/billing.go` selects Paddle when `MITOS_CONSOLE_PADDLE_API_KEY` + `MITOS_CONSOLE_PADDLE_WEBHOOK_SECRET` are set. Durable Postgres usage store landed (#211). Customer mapping and spend-cap store are still in memory.
- Console SPA (`web/app`): capability-gated routes for Overview (Instruments), Sandboxes, Fork tree, Templates, Secrets, API keys, Members, Projects, Audit, Retention, Roles, Usage, Billing, Settings. `firstrun/` foundation from the prior session: a content registry and a guided first-run card, with the `?uc` seed carried signup -> verify -> console.

## What is missing (the work)

1. Get Started is dark and ungated: CTAs point at the quickstart; signup is off; no allowlist gate.
2. Email confirmation exists but `normalizeEmail` only trims and lowercases: no Gmail dot/plus/googlemail folding, no disposable-domain block, no velocity cap. The $5 credit can be farmed with address variants.
3. The first-run is a static card, not the event-driven, tabbed, "waiting for your first call then confetti" experience.
4. No way to buy credits: the ledger can record a top-up, but there is no checkout on the provider seam and no "Add credits" UI. Spend-cap controls (ws4 Task 2) and the Paddle checkout (ws4 Task 3) were never built.
5. Available credits are not first-class in Overview.
6. Dashboard polish (`feat/console-ux-polish`: PageHeader, top bar, account menu, command palette) is unmerged, and the per-view DX bar is uneven.

## Principles inherited

Master journey rules apply: no dead ends; simple surface, depth one click down; the aha is intent-shaped. Plus the repo canon: no em or en dashes anywhere; Fluorescence tokens only in chrome, `@mitos/brand` components; approved numbers only (~27 ms fork, ~3 MiB per fork, Firecracker <125 ms boot), never the refuted demand-gap stat or blanket "sub-second boot"; money is integer cents; provider keys, webhook secrets, and verification tokens are never logged; TDD with the test in the same commit; DCO sign-off; conventional commits; stage explicit paths only.

## Architecture and workstreams

### Base and integration (plan step 0)

Work in the isolated worktree `mitos-run/mitos-journey` (branch `feat/hosted-journey-finish`), branched from `origin/main`. Integrate the prior WIP onto it in this order, running the suites after each: `fix/215-onboarding-verify-idempotent` (small hardening), `hosted-launch-ws4` (ws3 first-run + `?uc` seed + ledger fix), `feat/console-ux-polish` (shell + PageHeader). Resolve conflicts against current `origin/main`. This yields a green base with the first-run card, the `?uc` seed, and the polished shell already present, on top of which the remaining work is built.

### A. Get Started live, behind an allowlist gate

Mirror paperclip.inc's gate shape, adapted to mitos's verify-then-provision flow (paperclip gates per-request at tenant resolution; mitos gates once, at verify).

- Website (`mitos-run/website`): flip `SIGNUP_BASE` to the app signup path; keep the `?uc` seed. A `/waitlist` page: calm "you are on the list" copy, on-brand, no dead end (link to docs and to sign out). Enable the use-cases nav for at least Home and the rollouts on-ramp.
- Allowlist store: a new `allowlist` table (email primary key, note, created_at) and an `IsAllowed(email)` check with precedence auto-allow-domain -> allowlist row. Auto-allow domains from `MITOS_CONSOLE_AUTOALLOW_DOMAINS` (default `mitos.run`). In-table means approved; no status enum. Both an in-memory and a Postgres implementation behind one interface, matching the existing store pattern.
- Gate placement: `SignUp` always records the pending signup and sends the verification email (ownership proof and the abuse choke point). `Verify` checks `IsAllowed(canonicalEmail)`:
  - allowed: provision org + signup credit + first key as today, redirect into the console first-run with `?uc`.
  - not allowed: mark the pending record as waitlisted, do not provision, return a `waitlisted` result. The verify SPA page shows the "you are on the list" state (or the website `/waitlist`). No org, no credit, no key.
- Approval path: an internal `POST /internal/approve-signup` handler on the console (`{email, note?}`) that inserts the allowlist row (idempotent) and sends the "you are in" email; a repo ops entry (script or workflow op) to invoke it, matching the paperclip approve-signup ergonomics. Approval is not self-serve.
- Emails: reuse the `EmailSender` seam. Add an approved template ("you are in, sign in to run your first fork"). No "you joined" email (silent join, matching paperclip).
- Config: `MITOS_CONSOLE_SIGNUP=1`, `MITOS_CONSOLE_SIGNUP_CREDIT_CENTS=500`, `MITOS_CONSOLE_AUTOALLOW_DOMAINS=mitos.run`. The signup stays gated for launch; flipping to truly open is a later decision gated on master WS6.

### B. Email confirmation and anti-abuse

- Canonical email: extend the onboarding email handling with a `canonicalEmail(addr)` that lowercases, and for Gmail/Googlemail strips dots in the local part, drops `+tag` sub-addressing, and folds `googlemail.com` to `gmail.com`. Non-Gmail providers get lowercase plus `+tag` strip is left provider-safe (only Gmail dot-stripping is Gmail-specific). Store the canonical form and uniquely index it, so `u.s.e.r+x@gmail.com` and `user@gmail.com` collapse to one identity and one signup credit. The display/delivery address keeps the original form.
- Disposable domains: a bounded blocklist checked at signup (JSON list in-tree, with a staff allow path). Unknown or disposable -> uniform 202 with no provisioning (no enumeration signal).
- Velocity: a per-IP signup cap (default 10/hour, env-tunable) using the existing rate-limit facility if present, else a small store. Fail closed on the cap, not on absence of config.
- Captcha: a Friendly Captcha verification hook at signup, pass-through when keys are absent (EU-sovereign, matching paperclip's choice). Optional at launch.
- Guarantee: the $5 credit is grantable at most once per canonical identity; the allowlist and the canonical-unique index together make trial-farming uneconomic.

### C. First-run aha, elevated

Build on the prior `firstrun/` registry and card; replace the static card body with the event-driven, tabbed guided run.

- Intent + runtime: shaped by `?uc` (content registry already keys on the slug). Within an intent, a tabbed runtime switch: Python, TypeScript, CLI. The tab selects the Step 2 snippet.
- Step 1, key: a masked, copyable `export MITOS_API_KEY=mk_live_abcd______` row (the first key shown once, masked to a visible prefix). A copy button; clicking it copies the full export line and checks Step 1.
- Step 2, run it: the per-tab code snippet in a mono block with a copy button; clicking it checks Step 2.
- Step 3, first call: a live "Waiting for your first call..." state that polls a new console signal until the org's first exec lands. The onboarding funnel already emits `first_sandbox_created` and `first_exec`; expose an org-scoped `firstActivity` read on the console BFF (derived from those events or from the instruments/sandboxes signal) that the SPA polls with backoff.
- The moment: when the signal flips, an on-brand success celebration (confetti honoring prefers-reduced-motion) and a transition to "you are live" with next steps (open the fork tree, view usage, add credits) and the fork tree beginning to light up.
- Trust loop: the credit and current spend are visible throughout ("$5.00 credit, $0.00 spent"), reinforcing the see-it-work-and-see-the-cost loop.
- Accessibility: real headings, real buttons with the magenta focus ring, aria-live step-completion and "copied" announcements, the accessible parallel table for the fork tree already exists. Tokens only, no dashes.

The in-console fork button (one-click fork-and-watch) remains a fast-follow that needs a create/fork BFF op on `SandboxControl` and the cluster; the snippet path is the launch aha.

### D. Credits in Overview and buy-credits via Paddle

- Available credits first-class: a persistent Overview element showing the current org's available credit and spend (not only inside the first-run card, and not only when `c.billing`). Reuse the billing read; when billing is disabled, show credit only.
- Spend-cap controls (resume ws4 Task 2): a `billing.manage`-gated `POST /console/billing/spend-cap` and the Billing view form (soft/hard cents, validated non-negative and hard >= soft). This is the guardrail that makes "instant" safe.
- Buy credits (resume and refocus ws4 Task 3): a prepaid top-up rather than a plan upgrade. Add `CheckoutURL(ctx, customerRef, amount)` (or a top-up-specific method) to the Paddle provider that creates a hosted checkout/transaction for a top-up amount; a `billing.manage`-gated `GET /console/billing/checkout?amount=<cents>` returning `{url}`; the Billing/Overview "Add credits" UI offering the `TopUpLadder` tiers plus a custom amount; the webhook path records a `KindTopUp` ledger entry keyed by the provider reference (idempotent). Card capture and management stay in Paddle's hosted checkout/portal (Paddle is Merchant of Record); we link, we do not build a card form.
- Money-safety durability: move the customer mapping and the spend-cap store to Postgres behind their interfaces (usage store is already durable). This closes the "runaway swarm bills unbounded" gap for the gated cohort and is a precondition for any later open flip.
- What I need from the owner: a Paddle Billing sandbox API key + webhook signing secret to build/test against (live later), and a choice of catalog products for the ladder vs. Paddle custom transaction amounts. Build proceeds against a fake Paddle server and the sandbox until provided.

### E. Dashboard UX/DX polish

- Fold in `feat/console-ux-polish` (PageHeader across views, global top bar, account menu, command palette) during integration.
- Then a per-view craft pass using the interface-design skill against the console system: consistent PageHeader usage, empty and loading states that read as a waiting canvas, keyboard and focus order, mobile responsiveness, copy in brand voice. Each view leaves tidier than found; no scope creep beyond the journey.

## Data flow (happy path)

Website CTA (`/signup?uc=rollouts`) -> console signup page -> `POST /onboarding/signup {email, uc}` (canonicalized, abuse-checked, 202) -> verification email -> user clicks verify -> `Verify` checks `IsAllowed(canonical)`; allowed -> provision org + $5 credit + first key, redirect `/?uc=rollouts` -> first-run: copy key (Step 1), copy snippet (Step 2), run it, first exec lands -> poll flips -> confetti + fork tree lights up -> Overview shows credit and spend -> Add credits opens Paddle checkout -> webhook -> `KindTopUp` -> balance updates. Not-allowed at verify -> waitlist state; admin approve-signup -> "you are in" email -> next verify/sign-in provisions.

## Testing

- Go: TDD per behavior. Canonical-email table tests (dot/plus/googlemail, non-Gmail safety, farming collapse); allowlist `IsAllowed` precedence and the Postgres round-trip; verify gate branches (allowed provisions, waitlisted does not); approve-signup idempotency and email send; Paddle checkout against an `httptest` fake (asserts bearer auth, parses the URL, no secret in errors); webhook -> `KindTopUp` idempotency; spend-cap validation and permission gate; durable customer/spend-cap store round-trips. Postgres-backed tests use the test DSN; `go build ./...` and both `golangci-lint` invocations (darwin and `GOOS=linux`) clean.
- SPA: Vitest + RTL. First-run: tab switch changes the snippet; copy checks Step 1 and Step 2; the polling "waiting" state and the celebration on the signal (mock the poll); no-`uc` default content; no dashes. Billing: spend-cap form posts and confirms; Add credits opens the checkout URL; Overview credit element renders from the billing read. `pnpm test` and `pnpm build` clean.
- Journey e2e: extend the hosted e2e harness where present to cover signup -> verify (allowed and waitlisted) -> first-run signal. The live fork tree filling with real forks and the live Paddle round trip are verified on a deployed cluster and a Paddle sandbox, not locally; those are called out, not asserted locally.

## Sequencing

0. Integrate prior WIP onto the worktree, green base.
1. C (elevate first-run) and D (credits in Overview, spend cap, buy-credits) as the loved surfaces.
2. A (allowlist gate + website flip + `/waitlist`) and B (canonical email + abuse) to make it safe to turn on.
3. E (per-view DX polish pass).
4. Enable the gated funnel in a deploy: `MITOS_CONSOLE_SIGNUP=1`, `$5` credit, auto-allow `mitos.run`, Paddle sandbox keys. Verify end to end on the live gated site with the allowlist. Public open flip stays deferred to master WS6.

Each shippable slice is its own commit series with tests, reviewed before merge to `main`.

## Non-goals

- Public open self-serve (`ModeOpen` without allowlist); gated only, deferred to master WS6 (isolation hardening).
- In-console one-click fork-and-watch (needs a create/fork BFF op + cluster); the snippet is the launch aha.
- Box and packaged pricing plans and reserved-pool provisioning; prepaid top-up is the launch money path.
- A custom card form; Paddle hosted checkout/portal owns card capture as Merchant of Record.
- Building a Stripe SDK path; Paddle is the selected provider.
- SSO/SCIM and the remaining enterprise-governance layer beyond what already exists.

## Open questions

- Paddle top-up shape: catalog products per ladder tier vs. custom transaction amounts (owner decision; affects the checkout call and the UI).
- App signup URL/domain the website CTA points at (the console origin vs. a path on the apex), to confirm against the frontdoor routing and deploy in the paperclip/mono infra repo.
- Whether to ship the Friendly Captcha hook on at launch or leave it pass-through until abuse is observed.

## Risks

- Rebasing three prior branches onto a moved `origin/main` can conflict; mitigated by integrating one at a time with the suites as the gate, and by the prior branches being close to main.
- Canonical-email normalization can wrongly collapse two legitimately distinct addresses if over-broad; mitigated by keeping dot-stripping Gmail-only and covering it with table tests.
- The first-run "first call" signal depends on the funnel events being emitted server-side for the hosted path; verify the signal fires before relying on the poll, and provide a manual "I ran it" fallback that still checks the real signal.
