# Console Revamp Master Plan (World-Class UX/DX)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Close the gap between the Fluorescence brand promise and the shipped console: brand-locked theming with an accessible light/dark toggle, human-readable durable audit, teammate invites, operate verbs (create/fork/live logs/run command), pricing shapes (Box + Team tier) with website corrections, an instance-operator plane, and sharper onboarding.

**Architecture:** All work lands in two repos: `mitos` (Go BFF `internal/saas/console` + React SPA `web/app` + brand pkg `web/packages/brand`) and `website` (Astro). Console changes flow through the existing seams: `Capabilities` doc for edition gating, `Store`/`pgstore` for persistence, `AuditRecorder` for events, RBAC `Permission` map for authz. One branch + PR per workstream, sequential, each off origin/main.

**Tech Stack:** Go 1.x (stdlib http, pgx via pgstore), React 18 + TanStack Router/Query + vitest, vanilla CSS tokens (`@mitos/brand`), Astro 6 (website).

## Global Constraints

- Brand: "Mitos" in prose; lowercase for identifiers. Never use em/en dashes in any copy.
- Fluorescence rules: tokens only from `web/packages/brand/src/tokens.css`; color = meaning; elevation by lightness ladder; both themes must hold WCAG 2.2 AA (token comments carry ratios).
- Single binary, no build-time edition fork: all gating via server-advertised `Capabilities` (`internal/saas/console/capabilities.go`).
- Engine stays Apache-2.0 feature-complete; hosted tiers gate hosted conveniences only.
- Audit events NEVER carry secret values (seams.go:141 invariant).
- Tests: `go test ./...` (repo root) and `cd web/app && npm run typecheck && npm test`. Website: `cd website && npm run build`.
- Commits: conventional commits, small and frequent. Each workstream = one PR.
- Every new UI surface: keyboard reachable, aria labels, visible focus ring (`--focus` pattern in base.css), honest empty/loading/error states.

---

## Workstream 1: Theme default, visible toggle, brand unification

**Files:**
- Modify: `web/app/src/appearance.ts` (default theme)
- Modify: `web/app/index.html` (pre-paint script)
- Create: `web/app/src/nav/ThemeToggle.tsx`
- Modify: `web/app/src/nav/TopBar.tsx` (mount toggle between Help and AccountMenu)
- Modify: `web/app/src/views/Settings.tsx` (appearance tab already has theme control; keep in sync, no change needed unless copy)
- Test: `web/app/src/nav/ThemeToggle.test.tsx`, extend `web/app/src/appearance.test.ts` if present
- Website: Create `website/scripts/check-brand-tokens.mjs`, Modify `website/package.json` (add `check:brand` to build), copy `tokens.css` from brand pkg if diverged

**Interfaces:**
- Produces: `<ThemeToggle />` React component, no props. Cycles explicit theme dark -> light -> system via `getAppearance()/setAppearance()` from `appearance.ts`.
- `DEFAULTS.theme` changes `'system'` -> `'dark'` (brand default per Fluorescence; kills the light-console-vs-dark-website handoff break).

Tasks:
- [ ] 1.1 Change `DEFAULTS.theme` to `'dark'` in `appearance.ts`; update index.html pre-paint script so NO stored theme means `data-theme="dark"` (not unset); keep `'system'` as a selectable value that removes the attribute. Test: fresh localStorage renders `documentElement.dataset.theme === 'dark'`.
- [ ] 1.2 Build `ThemeToggle`: a single button, `aria-label="Theme: dark. Activate for light."` (dynamic), icon = sun/moon/auto glyph inline SVG using `currentColor`, cycles dark -> light -> system; persists via `setAppearance`; visible focus ring; test cycling + aria updates.
- [ ] 1.3 Mount in `TopBar.tsx` before `<AccountMenu/>`. Verify both themes AA: link color, `t-dim`, StatusDot on `--field` and light field (tokens already annotated; spot-check with axe or manual ratio calc in test comments).
- [ ] 1.4 Website tokens guard: script fetches nothing at build (no network); instead vendored checksum: commit the current sha256 of `web/packages/brand/src/tokens.css` into `website/scripts/brand-tokens.sha256` and CI-fail with a helpful message when `src/styles/tokens.css` hash differs from recorded upstream hash. Reconcile the ONE divergence first: port website `--text-*` semantic aliases into brand `tokens.css` (additive; console keeps `--step-*`), then byte-sync website copy from brand.
- [ ] 1.5 Commit(s) + PR "feat(console): dark brand default + accessible theme toggle; brand token sync guard".

## Workstream 2: Audit log: durable, human-readable

**Files:**
- Modify: `internal/saas/console/seams.go` (AuditEvent fields), `internal/saas/console/console.go` (audit() helper + call sites), `internal/saas/console/account.go`, `internal/saas/console/secrets.go`
- Create: `internal/saas/pgstore/auditlog.go` (+ migration in `internal/saas/pgstore/migrate.go`)
- Modify: `internal/saas/console/memseams.go` (MemAuditLog carries new fields; add cap/limit)
- Modify: `internal/saas/console/console.go` handleListMembers -> include email/display_name in MemberView
- Modify SPA: `web/app/src/api.ts` (AuditEvent type), `web/app/src/views/Audit.tsx`, `web/app/src/views/Instruments.tsx` (RecentActivityPanel), create `web/app/src/lib/auditText.ts` + `web/app/src/lib/dates.ts`
- Test: Go table tests per store impl; `auditText.test.ts`, date util tests

**Interfaces:**
- `AuditEvent` gains: `ActorName string \"json:\\\"actor_name\\\"\"`, `ActorType string` (`user|api_key|system`), `TargetType string` (`session|key|sandbox|member|project|secret|sink|profile|org`), `TargetName string`. Written best-effort at record time via account lookup (`Store.GetAccount`); never fails the action.
- `auditText.ts` exports `renderAuditSentence(e: AuditEvent, selfAccountID: string): {actor: string, verb: string, object: string}` with a template map per action (e.g. `session.revoke_all` -> actor + "signed out of all sessions" and NO object; `member.role` -> "changed <target>'s role"). Unknown actions fall back to `action` code.
- `dates.ts` exports `fmtRelative(iso: string): string` and `fmtAbsolute(iso: string, locale?: string, tz?: string): string`; all views migrate to it; locale/timezone read from `useAccount()` when set, else browser.

Tasks:
- [ ] 2.1 Extend `AuditEvent` struct + update every `c.audit(...)` call site to pass actor name (lookup once per request via caller account) and target type/name; `session.revoke_all` and `profile.update` set `Target=\"\"`, `TargetType=\"session\"|\"profile\"` (kill the self-target duplication). Go tests: recorded event carries names.
- [ ] 2.2 Postgres persistence: `audit_events` table (org_id, at, actor_id, actor_name, actor_type, action, target, target_type, target_name, detail; index (org_id, at desc)); implement `billing`-style pgstore adapter satisfying `AuditRecorder`; wire selection in `cmd/console` (pg when store is pg, else mem). Migration + round-trip test.
- [ ] 2.3 MemberView: add `email`, `display_name` (server joins accounts); update Members.tsx to show name + email instead of raw account id.
- [ ] 2.4 SPA: sentence rendering in RecentActivityPanel ("You signed out of all sessions", relative time) and Audit table (Actor column = name with id in tooltip/mono subline; Action column keeps `category.operation` code as dim mono badge for machine-grep parity). Shared `dates.ts` replaces all scattered `toLocale*` calls (Settings, Members, Keys, Templates, Projects, Billing).
- [ ] 2.5 Commit + PR "feat(audit): durable postgres audit log with human-readable events".

## Workstream 3: Invites + member management

**Files:**
- Create: `internal/saas/invites.go` (entity + service), `internal/saas/pgstore/invites.go` (+ migration), mem impl in `internal/saas/store.go`/memstore
- Modify: `internal/saas/store.go` (Store interface: CreateInvitation/ListInvitations/GetInvitationByTokenHash/UpdateInvitationState/DeleteMembership/RemoveInvitation), `internal/saas/model.go`
- Modify: `internal/saas/onboarding/service.go` + `smtp.go` (EmailSender gains `SendInvite(ctx, email, org, inviter, token)`), signup Verify auto-joins org when pending invite matches verified email
- Modify: `internal/saas/console/console.go` (routes: `POST /console/invites`, `GET /console/invites`, `DELETE /console/invites/{id}`, `POST /console/invites/{id}/resend`, `DELETE /console/members/{accountID}`), new `internal/saas/console/invites.go`
- Create: public accept route `GET/POST /invite/accept` (session-mediated: logged-out -> login/signup flow -> accept)
- SPA: Modify `web/app/src/views/Members.tsx` (Invite button + modal, Pending section with resend/revoke, Remove member), `web/app/src/api.ts`, `web/app/src/data/org.ts`; Create `web/app/src/views/members/InviteModal.tsx`, accept page `web/app/src/auth/AcceptInvite.tsx` (pre-auth + post-auth route)
- Test: Go invite lifecycle table tests (create/accept/expire/revoke/resend, last-owner, dup-email), SPA modal test

**Interfaces:**
- `Invitation{ID, OrgID, Email, Role, TokenHash, State (pending|accepted|expired|revoked), InviterID, CreatedAt, ExpiresAt}`; expiry 7 days; raw token only in email link (`/invite/accept?token=...`), sha256 stored.
- Authz: create/revoke/resend require `PermManageMembers`; default role `member`; audit actions `invite.create|invite.revoke|invite.resend|invite.accept|member.remove`.
- Accept semantics: session required; email match rule = exact match OR same verified corporate domain (consumer domains exact only, list gmail/outlook/yahoo/icloud/proton); accepting adds Membership and marks accepted; signup with pending invite auto-joins after verification instead of minting only the personal org.
- Remove member: forbidden on last owner (reuse ErrLastOwner); removing self allowed unless last owner.

Tasks:
- [ ] 3.1 Entity + store (mem first, TDD lifecycle incl. expiry) + pgstore migration.
- [ ] 3.2 Console endpoints + RBAC + audit events + rate limit (50 invites/24h/org).
- [ ] 3.3 Email: SendInvite via existing SMTP sender (plain, on-brand text); base URL from existing VerifyBaseURL config sibling `InviteBaseURL`.
- [ ] 3.4 Accept flow: BFF handler + SPA accept page for both logged-in and fresh-signup paths; onboarding Verify hook for auto-join.
- [ ] 3.5 Members UI: Invite modal (email list textarea, role select, send), Pending invites section (state, expiry, resend/revoke), Remove member with confirm; empty state becomes an invite CTA.
- [ ] 3.6 Commit + PR "feat(teams): email invitations and member management".

## Workstream 4: Operate verbs (create, fork, live logs, run command)

**Files:**
- Modify: `internal/saas/console/console.go` + `internal/saas/console/seams.go` (SandboxOps seam grows Create/Fork/Exec/StreamLogs; follow the existing terminate seam pattern), impls in the existing sandbox adapter (find via handleTerminate wiring in cmd/console)
- Routes: `POST /console/sandboxes` (template, vcpus, mem_gib, project_id), `POST /console/sandboxes/{id}/fork` (count<=16), `GET /console/sandboxes/{id}/logs/stream` (SSE, text/event-stream), `POST /console/sandboxes/{id}/exec` ({cmd, timeout_s<=60} -> {stdout, stderr, exit_code})
- SPA: Create `web/app/src/views/sandboxes/NewSandboxModal.tsx`, fork actions in `views/forktree/ForkTree.tsx` (node click -> side panel with Fork/Open) and `SandboxList.tsx` toolbar "New sandbox" primary button; SandboxDetail: Logs tab gains Live toggle (EventSource), Terminal tab becomes RunCommand panel (single command, output block, honest copy "full PTY is coming; this runs one command via exec"), fork action on detail header
- Test: Go handler tests with fake seam; SPA component tests for modal validation + SSE hook (`useLogStream`)

**Interfaces:**
- `api.ts` gains: `createSandbox(req): Promise<Sandbox>`, `forkSandbox(id, count): Promise<{ids: string[]}>`, `execSandbox(id, cmd): Promise<ExecResult>`, `logStreamURL(id): string`.
- Capability: reuse existing `caps` (no new gate; RBAC `resources.use` guards mutations server-side).
- Audit: `sandbox.create`, `sandbox.fork`, `sandbox.exec` (detail = cmd first 80 chars, never env/secrets).

Tasks:
- [ ] 4.1 Seam + fake + handler TDD for create/fork/exec/stream (SSE flush loop with ctx cancel).
- [ ] 4.2 Wire real adapter (whatever backs terminate today) for create/fork; exec via existing agent exec path (mitos sandbox exec transport); stream via existing logs source with follow.
- [ ] 4.3 SPA: New-sandbox modal (template select from useTemplates, vcpu/mem selects bounded by quota tier, optional project), optimistic list insert; empty states across Sandboxes/ForkTree/Overview get the real "New sandbox" / "Fork" CTAs.
- [ ] 4.4 Fork tree interactivity: node select -> panel (id, phase, private/shared bytes, buttons Fork n / Open / Terminate); keyboard: nodes focusable, Enter opens panel (the table remains SR source of truth).
- [ ] 4.5 Live logs (EventSource with auto-reconnect + pause on hidden tab) + RunCommand panel.
- [ ] 4.6 Commit + PR "feat(console): create, fork, exec, and live logs from the dashboard".

## Workstream 5: Pricing shapes + website truth

**Files (mitos):**
- Create: `internal/saas/billing/plans.go` (Plan{key, name}, `PlanFree|PlanTeam`, Entitlements{SSOEnforced, SCIM, AuditStreaming, AuditRetentionDays, SeatPriceCents}; org -> plan lookup seam with static default Free), Create `internal/saas/billing/reservation.go` (Box: Reservation{VCPU, MemGiB, MonthlyCents}; monthly ledger grant `box_grant` + discount math; catalog: Box S 2vCPU/4GiB $19, Box M 4/8 $39, Box L 8/16 $75, all ~30% under PAYG list; constants marked ILLUSTRATIVE like DefaultRates)
- Modify: `internal/saas/console/capabilities.go` (advertise `plan` + entitlements), gate audit sink creation on AuditStreaming entitlement (hosted only; community edition keeps everything on)
- SPA: Billing.tsx shows plan card + Box selector (purchase via existing portal/topup seam when available, else honest "contact"/coming state)
- Website: Modify `src/pages/pricing.astro`: three shapes (PAYG hero unchanged, Box cards, Team seat tier $20/user/mo listing SSO enforcement, SCIM, audit streaming, retention); fix credit copy to $100; egress row -> "$0.09/GiB (illustrative)" replacing "Free"; FAQ entry on self-host parity (engine keeps all features)
- Test: Go entitlement gating tests, reservation grant idempotency test (one grant per org-month)

Tasks:
- [ ] 5.1 plans.go + entitlements + capabilities advertisement + community-edition override (everything on when self-hosted). TDD gating helper.
- [ ] 5.2 reservation.go ledger grants (idempotent key `box|org|YYYY-MM`), quota-tier bump hook.
- [ ] 5.3 Sink-creation gate + Billing UI plan/box cards.
- [ ] 5.4 Website pricing page: shapes, credit $100, metered egress, sso.tax-clean framing (basic SSO login stays free everywhere; Team gates enforcement/SCIM/streaming). Build passes.
- [ ] 5.5 Commits + 2 PRs (mitos, website).

## Workstream 6: Operator plane (/admin)

**Files:**
- Create: `internal/saas/console/admin.go` (endpoints `GET /console/admin/overview|orgs|nodes|waitlist`, `POST /console/admin/waitlist/{id}/approve`), instance-admin check: account email in `MITOS_CONSOLE_INSTANCE_ADMINS` (comma list) OR community edition + owner of the only org; capability `admin: bool`
- Data: orgs rollup from Store + usage source (reuse /console/usage internals per org); nodes via existing k8s client used by controller-side listing (read-only NodeList: name, labels mitos.run/kvm|dedicated, allocatable cpu/mem, ready)
- SPA: routes `/admin`, `/admin/orgs`, `/admin/nodes`, `/admin/waitlist` in a new nav group `Operate` (visible when `caps.admin`), views under `web/app/src/views/admin/`
- Test: authz tests (non-admin 403), rollup shape test, SPA nav gating test

Tasks:
- [ ] 6.1 Capability + authz middleware + overview endpoint (org count, running sandboxes, nodes ready, signup mode). TDD.
- [ ] 6.2 Orgs table (id, name, tier, members, running, month usage cents) + waitlist list/approve (flips PendingSignup -> invite email path from WS3).
- [ ] 6.3 Nodes view (k8s read; graceful "not available in this deployment" when no kubeconfig).
- [ ] 6.4 SPA Operate section + views (dense tables, StatTiles, same PageHeader pattern).
- [ ] 6.5 Commit + PR "feat(admin): instance operator plane".

## Workstream 7: Onboarding sharpening + agent-native

**Files:**
- Modify: `web/app/src/views/firstrun/FirstRun.tsx` + `content.ts`: pre-checked step 0 ("Account created"), waiting state gains 90s timeout -> troubleshooting panel (checklist: key copied? egress? correct host api.mitos.run) + synthetic trigger snippet (`mitos run \"echo hello\"` or curl), celebration = fork-tree glyph lights up + link "See your fork tree"
- Create: `web/app/src/views/firstrun/InviteNudge.tsx`: after first activity, if `caps.teams` and members==1, Overview shows one-time dismissable "Bring your team" card (localStorage dismiss)
- Website: Create `public/llms.txt` (product summary, docs links, API base https://api.mitos.run, auth, quickstart, MCP note) + ensure robots serves it; add "For agents" docs stub page linking llms.txt
- Test: FirstRun timeout state test, nudge dismiss test

Tasks:
- [ ] 7.1 FirstRun: endowed progress + timeout/troubleshoot + synthetic trigger. TDD timer with fake clock.
- [ ] 7.2 InviteNudge card wired to members count + dismiss persistence.
- [ ] 7.3 llms.txt + For-agents page on website (build passes).
- [ ] 7.4 Commits + PRs (mitos, website).

## Workstream 8: Mobile experience pass

**Files:** `web/packages/brand/src/base.css` (responsive rules), per-view tweaks in `web/app/src/views/**`, `web/app/src/nav/*`; website: spot-check `src/styles/base.css` + key pages.

Principles: the console must be genuinely usable at 375px width, not merely not-broken. Simplicity first: fewer columns, bigger targets, no horizontal page scroll ever.

Tasks:
- [ ] 8.1 Audit at 375/768px: every `.tbl` table wraps in an `overflow-x: auto` container (page body never scrolls horizontally); StatTile grids collapse to 1-2 columns; PageHeader actions wrap; forms full-width; modals full-screen sheet style on small viewports.
- [ ] 8.2 Touch targets: all interactive chrome (nav links, toggle, row actions, palette items) >= 44px hit area on touch/coarse pointers; command palette usable on mobile (visible trigger, since Cmd-K does not exist on phones).
- [ ] 8.3 Fork tree on touch: nodes tappable (min target), panel opens as bottom sheet; the accessible table remains the fallback.
- [ ] 8.4 Website spot-check at 375px: nav, pricing table, code blocks scroll in-container. Fix only clear breakages.
- [ ] 8.5 Verification: vitest + typecheck; manual viewport screenshots via browser tooling if available; commit.

## Workstream 9: One-click feedback with diagnostics

**Files:** Create `web/app/src/nav/FeedbackButton.tsx` (+ dialog), `web/app/src/lib/diagnostics.ts`; Modify `web/app/src/nav/TopBar.tsx`, `internal/saas/console/capabilities.go` (add `feedback: {channel: "email"|"github", target: string}`), `cmd/console/capabilities.go` wiring (env `MITOS_CONSOLE_FEEDBACK_EMAIL`, default github target `mitos-run/mitos`).

**Interfaces:** `collectDiagnostics(caps): string` returns a plain-text block: console version (capabilities-advertised if present), edition, current route, browser UA, viewport, theme, org id, timestamp (UTC). NEVER keys, tokens, emails of other users, or sandbox contents.

Tasks:
- [ ] 9.1 Capabilities: feedback channel doc; hosted default email `feedback@mitos.run`, community default GitHub new-issue URL. TDD Go test on capabilities JSON.
- [ ] 9.2 `FeedbackButton` in TopBar (speech-bubble icon button, aria-labelled): opens a small dialog: one textarea, a visible preview of attached diagnostics (transparency; user sees exactly what is sent), submit = `mailto:` with subject/body prefilled (email channel) or `https://github.com/<target>/issues/new?title=&body=` (github channel). No backend write path in v1.
- [ ] 9.3 Tests: diagnostics contains route/edition and NEVER matches /key|token|secret/i fixtures; channel switch follows caps; dialog a11y (focus trap, Escape).
- [ ] 9.4 Version in the sidebar footer: capabilities doc advertises `version` (server build version, wired from the existing build-info/ldflags source if present, else "dev"); render under the OwnershipBadge as a dim mono line (`--ink-3`, click copies "mitos <version> (<edition>)"), and include it in feedback diagnostics.
- [ ] 9.5 Commit.

---

## Execution protocol (the loop)

For each workstream in order 1..7: dispatch implementer subagent(s) per task with this plan section + relevant file excerpts; run `go test ./...` and `cd web/app && npm run typecheck && npm test` (and website build for website tasks); review the diff myself for brand/a11y/API-shape fidelity; fix or re-dispatch; commit; open PR; move on. After WS7: final sweep (typecheck, tests, `npm run build` both apps), then report with PR links.
