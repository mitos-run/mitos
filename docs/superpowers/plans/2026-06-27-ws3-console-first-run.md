# Workstream 3: console first-run experience Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development. Steps use checkbox (`- [ ]`) syntax.

**Goal:** The job-done moment. Right after a new user lands in the console with zero activity, give them a guided, use-case-shaped first-run: the runnable fork-a-swarm snippet, their free credit, and a live canvas (fork tree + instruments) that lights up as their forks appear, so the aha is "I forked a swarm and watched it diverge, and I can see the cost."

**Architecture:** The console cannot create or fork sandboxes itself yet (the SDK/CLI is the create path; `SandboxControl` is list/get/terminate only). So first-run is a guided card on the Overview, shown when the org has no measured activity, that hands over the snippet and points at the live fork-tree and instruments which poll real data. The use-case slug (`?uc`) is carried from signup through verify to the console so the snippet and copy are intent-shaped. An in-console fork button is a fast-follow that needs a create/fork BFF op and the cluster.

**Tech Stack:** React 18 + TanStack Router/Query (web/app), Vitest + RTL, `@mitos/brand`, Go (`internal/saas/onboarding`, `internal/saas` account model) for the `?uc` seed.

## Global Constraints

- No em (U+2014) or en (U+2013) dashes anywhere. ASCII hyphen only.
- Fluorescence tokens only in chrome (no hardcoded hex). `@mitos/brand` components. Brand voice: concrete, confident, imperative CTAs. Approved numbers only (~27 ms, ~3 MiB, Firecracker <125 ms); never the refuted demand-gap stat.
- Go: `fmt.Errorf("ctx: %w", err)`; gofmt + `go vet` clean; secrets/tokens never logged.
- DCO: every commit `git commit -s`. Conventional prefixes. Stage explicit paths only.
- Work in the ISOLATED WORKTREE /Users/jannesstubbemann/repos/mitos-run/mitos-ws2 (branch hosted-launch-journey). SPA commands from `web/app` (`pnpm test`, `pnpm build`); Go from the worktree root.
- Mirror existing patterns: `web/app/src/views/Instruments.tsx` (the Overview + its empty states + `useInstruments`/`useSandboxes`/`useBilling`), `web/app/src/auth/Verify.tsx` (the post-verify redirect + the snippet/copy pattern from the auth pages), `web/app/src/ui/{Card,EmptyState,StatTile,PageHeader}.tsx`, `web/app/src/data/usecases`-style content (port the rollouts snippet), and `internal/saas/onboarding` (signup/verify) for the `?uc` seed.

---

### Task 1: First-run content registry (intent-shaped)

A small typed registry mapping a use-case slug to the first-run content (headline, the runnable snippet, the one-line "what you will see"), reusing the marketing use-case voice. Pure data + a getter, vitest-tested.

**Files:**
- Create: `web/app/src/views/firstrun/content.ts`
- Test: `web/app/src/views/firstrun/content.test.ts`

**Interfaces:**
- Produces: `type FirstRunContent = { slug: string; title: string; lede: string; snippet: string; watchFor: string }`, `const FIRST_RUN: FirstRunContent[]`, `getFirstRun(uc?: string): FirstRunContent` (returns the matching entry or a generic default).

- [ ] **Step 1: Write the failing test**

`content.test.ts`: assert `getFirstRun('rollouts')` returns the rollouts entry with a snippet containing `fork(`, that `getFirstRun(undefined)` and `getFirstRun('nope')` return the generic default (a non-empty title + snippet), and that NO entry contains an em or en dash (reuse the dash regex `/[–—]/`).

- [ ] **Step 2: Run to verify it fails**

Run: `cd web/app && pnpm test -- firstrun/content` (FAIL: module missing).

- [ ] **Step 3: Implement the registry**

Create `content.ts` with at least the `rollouts` entry (port the snippet from the marketing rollouts use case: a Python `fork` snippet) and a generic default (`{ slug: 'default', title: 'Fork your first swarm', lede: 'Fork one warm sandbox into many isolated runs. Each is its own microVM, alive in ~27 ms.', snippet: <the python fork snippet>, watchFor: 'Your fork tree and live metrics light up here as the forks appear.' }`). Add a `code-execution` and an `evals` entry if quick; the default covers the rest. `getFirstRun` matches by slug, falls back to default. No dashes.

- [ ] **Step 4: Run to verify it passes, then commit**

Run: `cd web/app && pnpm test -- firstrun/content`
Expected: PASS.

```bash
git add web/app/src/views/firstrun/content.ts web/app/src/views/firstrun/content.test.ts
git commit -s -m "feat(console): first-run content registry shaped by use case"
```

---

### Task 2: First-run guided card on the Overview

Show a guided first-run card at the top of the Overview when the org has no measured activity, intent-shaped by `?uc`, with the runnable snippet, the credit, and the live-canvas pointer. Improve the empty states to feel like a waiting canvas.

**Files:**
- Create: `web/app/src/views/firstrun/FirstRun.tsx`
- Modify: `web/app/src/views/Instruments.tsx` (render `<FirstRun/>` above the panels when first-run applies)
- Test: `web/app/src/views/firstrun/FirstRun.test.tsx`

**Interfaces:**
- Consumes: `getFirstRun` (Task 1); `useInstruments`, `useSandboxes`, `useBilling` (existing hooks); the `?uc` query param (read via the router search or `window.location.search`); `@mitos/brand` Card/Button + the copy-button pattern from `web/app/src/auth/Verify.tsx`.
- Produces: a `<FirstRun/>` component and a small `isFirstRun(instruments, sandboxes)` predicate (true when `forks_served === 0` and there are no live sandboxes).

- [ ] **Step 1: Write the failing test**

`FirstRun.test.tsx`: render `<FirstRun uc="rollouts" />` (allow a `uc` prop for tests), assert it shows the rollouts title, a code block containing `fork(`, a copy button, the free-credit line, and a pointer to the fork tree / live metrics. Add a case with no `uc` showing the default title. Mock the hooks (mirror how Instruments tests or other view tests mock `useBilling`/`useInstruments`; read an existing view test for the mock pattern).

- [ ] **Step 2: Run to verify it fails**

Run: `cd web/app && pnpm test -- firstrun/FirstRun` (FAIL).

- [ ] **Step 3: Implement FirstRun**

Create `FirstRun.tsx`: a brand Card with the use-case `title` + `lede`, the `snippet` in a mono/Terminal block with a Copy button (reuse the Verify page copy approach: Clipboard API, "Copied" announced via aria-live, failure feedback), a line surfacing the free credit and current spend (from `useBilling`: "$5 credit, $0.00 spent. Watch your spend grow as you fork."), and the `watchFor` pointer (a link to the Fork tree route and the metrics on this page). Accessible: real headings, copy button is a real button with the magenta focus ring, prefers-reduced-motion. Tokens only. No dashes. Export `isFirstRun(instruments, sandboxes)`.

- [ ] **Step 4: Wire into Instruments**

In `Instruments.tsx`, compute `isFirstRun(instruments.data, sandboxes.data)` and, when true, render `<FirstRun uc={ucFromSearch} />` above the existing panels (read `?uc` from the router search or window.location). Keep the existing panels (they show their own empty states beneath, which now read as the waiting canvas). Do not change behavior for an active org (forks_served > 0 hides the card).

- [ ] **Step 5: Run tests + build, then commit**

Run: `cd web/app && pnpm test && pnpm build`
Expected: PASS, clean.

```bash
git add web/app/src/views/firstrun/FirstRun.tsx web/app/src/views/firstrun/FirstRun.test.tsx web/app/src/views/Instruments.tsx
git commit -s -m "feat(console): guided first-run card on the overview"
```

---

### Task 3: Carry the use-case seed from signup through verify to the console

Make `?uc` survive the email round trip so the first-run is intent-shaped even when the verify link is clicked later. Store the slug on signup, return it at verify, and have the verify page carry it into the console redirect.

**Files:**
- Modify: `internal/saas/onboarding/service.go` + `http.go` (accept and persist `uc` on signup; return it on verify)
- Modify: `internal/saas/onboarding/service.go` `PendingSignup` (add a `UseCase` field) and the stores (Mem + Pg)
- Modify: `internal/saas/pgstore/migrations` (a new migration adding a `use_case` column to `pending_signups`) and `pendingstore.go`
- Modify: `web/app/src/auth/Signup.tsx` (send `uc` from the query param in the signup POST), `web/app/src/auth/Verify.tsx` (append `?uc=<from response>` to the Continue-to-console link)
- Tests: extend the onboarding service/http tests and the Signup/Verify tests.

**Interfaces:**
- Consumes: the existing signup/verify flow; the `?uc` query param on the Signup page.
- Produces: signup request `{ email, uc? }`; `PendingSignup.UseCase`; verify response gains `useCase`; the console redirect `/?uc=<slug>`.

- [ ] **Step 1: Write the failing Go test**

In the onboarding test, assert that `SignUp` with a use-case persists it on the pending signup and that `Verify` returns it in the result. Mirror the existing onboarding test setup. Validate `uc` (kebab-case, bounded length; ignore/empty unknown values rather than erroring).

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/saas/onboarding/ -run UseCase -v` (FAIL).

- [ ] **Step 3: Implement the Go seed**

Add an optional `uc` to the signup request (`http.go` decodes it; validate kebab-case, max length, drop if invalid), thread it to `Service.SignUp` -> `PendingSignup.UseCase`, persist in Mem + Pg pending stores (new migration `0003_pending_use_case.sql` adding `use_case TEXT NOT NULL DEFAULT ''`; update `PutPending`/scan), and include it in `VerifyResult` + the verify JSON response (`useCase`). Never log it as anything sensitive (it is not, but keep logs id/count only).

- [ ] **Step 4: Run the Go test (with the test Postgres), build, vet**

Run: `export MITOS_TEST_DATABASE_DSN="postgres://postgres:test@127.0.0.1:55432/mitos?sslmode=disable" && go test ./internal/saas/onboarding/ ./internal/saas/pgstore/ -v && go build ./... && go vet ./internal/saas/...`
Expected: PASS, clean.

- [ ] **Step 5: Wire the SPA**

In `Signup.tsx`: read `?uc` from the query and include it in the `/onboarding/signup` POST body. In `Verify.tsx`: read `useCase` from the verify response and make the "Continue to console" link `/?uc=<useCase>` (omit when empty). Update the Signup/Verify tests to assert the uc is sent and carried.

- [ ] **Step 6: Run SPA tests + build, then commit**

Run: `cd web/app && pnpm test && pnpm build`
Expected: PASS, clean.

```bash
git add internal/saas/onboarding/ internal/saas/pgstore/ web/app/src/auth/Signup.tsx web/app/src/auth/Verify.tsx web/app/src/auth/Signup.test.tsx web/app/src/auth/Verify.test.tsx
git commit -s -m "feat(onboarding): carry the use-case seed from signup through verify to the console"
```

---

## Self-Review

**1. Spec coverage (WS3 first-run):** the guided first-run aha shaped by use case (Tasks 1, 2), the live canvas pointer + cost trust-loop on the Overview (Task 2), and the `?uc` seed surviving the email round trip (Task 3). The in-console fork button (one-click fork-and-watch) is explicitly a fast-follow that needs a create/fork BFF op on `SandboxControl` plus the cluster; noted, not built here. The live fork-tree data is real but only populates once the user runs the SDK against a deployed cluster.

**2. Placeholder scan:** components are specified by behavior, the content registry is concrete, the snippet is the marketing rollouts snippet ported, and the Go seed names the exact fields/migration. The hook-mocking in Task 2 references reading an existing view test for the pattern (intentional reuse), not a placeholder.

**3. Consistency:** `getFirstRun`/`FIRST_RUN` (Task 1) are consumed by `FirstRun.tsx` (Task 2); the `?uc` the Signup sends (Task 3) is the slug the first-run content keys on (Task 1) and the console reads (Task 2). `PendingSignup.UseCase` and the `0003` migration column align across Mem/Pg stores and the verify response.

Note for the executor: Tasks 1-2 are SPA (vitest). Task 3 spans Go (needs the test Postgres for the pg pending store) and the SPA. The live fork-tree filling with real forks is verified on a deployed cluster, not locally; the locally verifiable surface is the guided card, the content, the cost line, and the seed plumbing.
