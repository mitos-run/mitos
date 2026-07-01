# Hosted journey slice 1: integrate prior WIP + elevate the first-run aha

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Integrate the prior killed-session WIP onto a fresh base, then lift the first-run from a static Python-only card into the event-driven, tabbed guided run: copy your key (checks), copy the snippet (checks), then a live "waiting for your first call" state that flips to an on-brand celebration when the first exec actually lands.

**Architecture:** Phase 0 rebases three prior branches (`fix/215`, `hosted-launch-ws4` which carries ws3, `feat/console-ux-polish`) onto `feat/hosted-journey-finish` (branched from `origin/main` @ 1.10.0), gating each on the suites. Workstream C then extends the existing `web/app/src/views/firstrun/` foundation and adds one new console BFF read (`/console/first-activity`) that the SPA polls. The first key is carried from the verify page into the first-run via ephemeral `sessionStorage` (shown once), with a create-a-key fallback.

**Tech Stack:** Go (`internal/saas/console`, `cmd/console`), React 18 + TanStack Router/Query (`web/app`), Vitest + React Testing Library, `@mitos/brand` Fluorescence tokens.

## Global Constraints

- No em (U+2014) or en (U+2013) dashes anywhere: source, comments, tests, Markdown, commit messages. ASCII hyphen only.
- Fluorescence tokens only in chrome; no hardcoded hex. `@mitos/brand` components. Brand voice: concrete, confident, imperative. Approved numbers only (~27 ms fork, ~3 MiB per fork, Firecracker <125 ms boot); never the refuted demand-gap stat, never blanket "sub-second boot".
- Go: `fmt.Errorf("ctx: %w", err)`; gofmt + `go vet` clean; both lints (`golangci-lint run --timeout=5m` AND `GOOS=linux golangci-lint run --timeout=5m`). API keys and verification tokens are NEVER logged, NEVER in errors, NEVER in condition messages. Log ids and counts only.
- Money is integer cents (`billing.Money`); never float for accounting.
- DCO: every commit `git commit -s`. Conventional prefixes (feat/fix/docs/refactor/test/chore). Stage explicit paths only; never `git add -A`.
- TDD: write the failing test first; the behavior change and its test land in the same commit.
- Work in the ISOLATED WORKTREE `/Users/jannesstubbemann/repos/mitos-run/mitos-journey` (branch `feat/hosted-journey-finish`). SPA commands from `web/app` (`pnpm test`, `pnpm build`); Go from the worktree root. A test Postgres DSN (if a task needs pgstore) is `postgres://postgres:test@127.0.0.1:55432/mitos_fresh?sslmode=disable` (recreate fresh; pgstore tests Open-before-truncate).

---

## Phase 0: Integrate the prior WIP (mechanical, suite-gated, not TDD)

Bring the prior session's committed work onto the fresh branch, one source at a time, running the suites as the gate after each. If a cherry-pick conflicts, resolve against current `origin/main` semantics (prefer main's version of any file main changed, then re-apply the prior intent), keeping the DCO sign-off (`git cherry-pick -s -x`).

- [ ] **Step 0.1: Confirm the base is green**

Run: `cd /Users/jannesstubbemann/repos/mitos-run/mitos-journey && go build ./... && cd web/app && pnpm install && pnpm test && pnpm build`
Expected: PASS, clean. This is the origin/main @ 1.10.0 baseline.

- [ ] **Step 0.2: Integrate the onboarding hardening (`fix/215`)**

Run: `git -C /Users/jannesstubbemann/repos/mitos-run/mitos-journey cherry-pick -s -x origin/main..fix/215-onboarding-verify-idempotent`
Then: `go test ./internal/saas/... && go build ./...`
Expected: PASS. (These are the half-provisioned-recovery and drawdown-replay fixes; small, likely clean.)

- [ ] **Step 0.3: Integrate ws3 first-run + ws4 ledger fix (`hosted-launch-ws4`)**

Run: `git -C /Users/jannesstubbemann/repos/mitos-run/mitos-journey cherry-pick -s -x origin/main..hosted-launch-ws4`
Resolve conflicts if any (most likely in `internal/saas/console/console.go`, `internal/saas/onboarding/*`, `web/app/src/views/Instruments.tsx`, `web/app/src/api.ts`); keep both the main change and the ws feature.
Then, with the test Postgres up: `export MITOS_TEST_DATABASE_DSN="postgres://postgres:test@127.0.0.1:55432/mitos?sslmode=disable"; go test ./internal/saas/... && go build ./... && cd web/app && pnpm test && pnpm build`
Expected: PASS. Confirm the files `web/app/src/views/firstrun/{content.ts,FirstRun.tsx}`, `web/app/src/auth/{Signup.tsx,Verify.tsx}`, and `internal/saas/pgstore/migrations/0003_pending_use_case.sql` are present.

- [ ] **Step 0.4: Integrate the shell polish (`feat/console-ux-polish`)**

Run: `git -C /Users/jannesstubbemann/repos/mitos-run/mitos-journey cherry-pick -s -x origin/main..feat/console-ux-polish`
Resolve conflicts (likely `web/app/src/nav/routes.tsx`, `web/app/src/views/Instruments.tsx`, `web/packages/brand/src/base.css`); keep the PageHeader adoption and the ws3 first-run render together.
Then: `cd web/app && pnpm test && pnpm build`
Expected: PASS. Confirm `web/app/src/ui/PageHeader.tsx` exists and the top bar renders.

- [ ] **Step 0.5: Record the integrated base**

Run: `go build ./... && cd web/app && pnpm build`
Expected: clean. No commit needed (cherry-picks already committed). Note the HEAD sha for the review checkpoint.

---

## Workstream C: elevate the first-run

### Task C1: per-runtime snippets in the content registry

Extend the registry so each use case carries a Python, a TypeScript, and a CLI snippet (the tabbed step 2), replacing the single `snippet` field. Keep `getFirstRun` and the fallback behavior.

**Files:**
- Modify: `web/app/src/views/firstrun/content.ts`
- Test: `web/app/src/views/firstrun/content.test.ts`

**Interfaces:**
- Produces: `type Runtime = 'python' | 'typescript' | 'cli'`; `type FirstRunContent = { slug: string; title: string; lede: string; snippets: Record<Runtime, string>; watchFor: string }`; `const RUNTIMES: { id: Runtime; label: string }[]`; `getFirstRun(uc?: string): FirstRunContent` (unchanged signature).

- [ ] **Step 1: Update the failing test**

In `content.test.ts`, replace snippet assertions with: `getFirstRun('rollouts').snippets.python` contains `fork(`; `.typescript` contains `fork(`; `.cli` contains `mitos`; `getFirstRun(undefined)` and `getFirstRun('nope')` return the default entry with all three runtimes non-empty; and NO snippet across all entries and runtimes contains an em or en dash (`/[–—]/`). Assert `RUNTIMES` has the three ids in order python, typescript, cli.

- [ ] **Step 2: Run to verify it fails**

Run: `cd web/app && pnpm test -- firstrun/content`
Expected: FAIL (type error / missing `snippets`).

- [ ] **Step 3: Implement per-runtime snippets**

In `content.ts`: add `export type Runtime = 'python' | 'typescript' | 'cli'` and `export const RUNTIMES: { id: Runtime; label: string }[] = [{ id: 'python', label: 'Python' }, { id: 'typescript', label: 'TypeScript' }, { id: 'cli', label: 'CLI' }]`. Change `FirstRunContent.snippet: string` to `snippets: Record<Runtime, string>`. For each entry give three snippets, porting the existing Python and adding the TypeScript and CLI equivalents, e.g. for rollouts:

```ts
const ROLLOUTS = {
  python: `from mitos import AgentRun\n\nsb = AgentRun().sandbox("python", ready=True)\nswarm = sb.fork(8)\nfor run in swarm:\n    run.exec(["python", "rollout.py"])\n`,
  typescript: `import { AgentRun } from "mitos"\n\nconst sb = await new AgentRun().sandbox("python", { ready: true })\nconst swarm = await sb.fork(8)\nawait Promise.all(swarm.map((run) => run.exec(["python", "rollout.py"])))\n`,
  cli: `mitos sandbox create --ready python\nmitos fork <sandbox-id> --count 8 \\\n  --exec "python rollout.py"\n`,
}
```

Do the same for `code-execution`, `evals`, and `default` (port the existing Python; write faithful TS and CLI equivalents; approved numbers only; no dashes). Keep `getFirstRun` and `DEFAULT_ENTRY` as-is.

- [ ] **Step 4: Run to verify it passes, then commit**

Run: `cd web/app && pnpm test -- firstrun/content && pnpm build`
Expected: PASS, clean.

```bash
git add web/app/src/views/firstrun/content.ts web/app/src/views/firstrun/content.test.ts
git commit -s -m "feat(console): per-runtime first-run snippets (python, typescript, cli)"
```

---

### Task C2: the first-activity signal (console BFF)

Expose an org-scoped read the first-run polls to know when the user's first exec actually landed. Derive it from the same signal the Overview already trusts (`forks_served > 0`, or any Running/terminated sandbox), so it is true exactly when there is real activity.

**Files:**
- Create: `internal/saas/console/firstactivity.go`
- Modify: `internal/saas/console/console.go` (mount `GET /console/first-activity`)
- Test: `internal/saas/console/firstactivity_test.go`

**Interfaces:**
- Consumes: the existing org-scoped `InstrumentsSource` / `SandboxControl` the console already holds (whatever `console.go` uses to compute `forks_served` and list sandboxes). Reuse them; do not add a new store.
- Produces: `type FirstActivityView struct { Active bool \`json:"active"\` }`; handler `GET /console/first-activity` returning `{ "active": bool }`, 200, session-scoped to the caller's org (mirror the existing `/console/instruments` handler's auth and org resolution).

- [ ] **Step 1: Write the failing test**

`firstactivity_test.go`: with a fake instruments source reporting `forks_served: 0` and no sandboxes, the handler returns `{"active": false}`; with `forks_served: 1` (or one Running sandbox), it returns `{"active": true}`; an unauthenticated request is rejected the same way the instruments handler rejects it. Mirror `console_test.go`'s handler-test setup (fake sources, a request with the org session context).

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/saas/console/ -run FirstActivity -v`
Expected: FAIL (handler missing).

- [ ] **Step 3: Implement the handler**

Create `firstactivity.go`: an `active(ctx, orgID)` helper that returns true when `instruments.forks_served > 0` OR any sandbox exists for the org (reuse the same source calls `/console/instruments` and `/console/sandboxes` use). The handler resolves the org from the session (mirror the instruments handler), calls `active`, and writes `FirstActivityView{Active: ...}`. No secrets; log org id + the boolean only. Mount `GET /console/first-activity` in `console.go` beside `/console/instruments`.

- [ ] **Step 4: Run to verify it passes, then commit**

Run: `go test ./internal/saas/console/ -run FirstActivity -v && go build ./... && go vet ./internal/saas/console/`
Expected: PASS, clean.

```bash
git add internal/saas/console/firstactivity.go internal/saas/console/console.go internal/saas/console/firstactivity_test.go
git commit -s -m "feat(console): first-activity signal for the guided first-run"
```

---

### Task C3: the SPA first-activity hook (polling with backoff)

**Files:**
- Modify: `web/app/src/api.ts` (add the fetch + type)
- Create: `web/app/src/data/firstActivity.ts` (the TanStack Query hook)
- Test: `web/app/src/data/firstActivity.test.ts`

**Interfaces:**
- Consumes: `GET /console/first-activity` (Task C2).
- Produces: `api.firstActivity(): Promise<{ active: boolean }>`; `useFirstActivity(enabled: boolean)` returning `{ data, isLoading }`, polling every 3000 ms while `enabled` and `!data?.active`, and stopping once `active` is true (set `refetchInterval` to a function returning `false` when active).

- [ ] **Step 1: Write the failing test**

`firstActivity.test.ts`: mock `api.firstActivity` to resolve `{active:false}` then `{active:true}` on the second call; render the hook via a QueryClient test wrapper (mirror an existing `data/*.test.ts`); assert it polls (advance timers, second call happens) and that `refetchInterval` returns `false` once `active` is true.

- [ ] **Step 2: Run to verify it fails**

Run: `cd web/app && pnpm test -- data/firstActivity`
Expected: FAIL (module missing).

- [ ] **Step 3: Implement the hook**

In `api.ts` add `export type FirstActivity = { active: boolean }` and `export async function firstActivity(): Promise<FirstActivity>` (GET `/console/first-activity`, mirror an existing GET in this file). Create `data/firstActivity.ts`:

```ts
import { useQuery } from '@tanstack/react-query'
import { firstActivity } from '../api'

export function useFirstActivity(enabled: boolean) {
  return useQuery({
    queryKey: ['first-activity'],
    queryFn: firstActivity,
    enabled,
    refetchInterval: (q) => (q.state.data?.active ? false : 3000),
  })
}
```

- [ ] **Step 4: Run to verify it passes, then commit**

Run: `cd web/app && pnpm test -- data/firstActivity && pnpm build`
Expected: PASS, clean.

```bash
git add web/app/src/api.ts web/app/src/data/firstActivity.ts web/app/src/data/firstActivity.test.ts
git commit -s -m "feat(console): useFirstActivity polling hook"
```

---

### Task C4: carry the first key from verify into the first-run (shown once)

Stash the one-time first key in `sessionStorage` at verify so the first-run can render the masked export line, then clear it after copy. Provide a create-a-key fallback when absent.

**Files:**
- Modify: `web/app/src/auth/Verify.tsx` (stash `firstKey` from the verify response before redirecting)
- Create: `web/app/src/views/firstrun/firstKey.ts` (get/clear helpers)
- Test: `web/app/src/views/firstrun/firstKey.test.ts`, extend `web/app/src/auth/Verify.test.tsx`

**Interfaces:**
- Consumes: the verify JSON response field carrying the raw first key (confirm the exact field name in the verify handler; the onboarding provision returns the first key once). If the response does not yet include it, add it in the Go verify response (a `firstKey` field returned exactly once) and cover it in the onboarding http test.
- Produces: `takeFirstKey(): string | null` (reads `sessionStorage['mitos.firstKey']`, deletes it, returns it) and `peekFirstKey(): string | null` (reads without deleting, for render); a `maskKey(key: string): string` returning a visible prefix plus a fixed dot run (e.g. `mk_live_a1b2` + `••••••••`).

- [ ] **Step 1: Write the failing test**

`firstKey.test.ts`: set `sessionStorage['mitos.firstKey'] = 'mk_live_a1b2c3d4e5'`; `peekFirstKey()` returns it and leaves it; `takeFirstKey()` returns it and subsequent `peekFirstKey()` is null; `maskKey('mk_live_a1b2c3d4e5')` starts with `mk_live_a1b2` and contains no raw tail characters after the prefix (only the prefix + dots). In `Verify.test.tsx`, assert that a successful verify with a `firstKey` in the response writes `sessionStorage['mitos.firstKey']` before the redirect.

- [ ] **Step 2: Run to verify it fails**

Run: `cd web/app && pnpm test -- firstrun/firstKey auth/Verify`
Expected: FAIL.

- [ ] **Step 3: Implement**

Create `firstKey.ts` with `peekFirstKey`, `takeFirstKey`, `maskKey` (prefix = first 12 chars, then `'•'.repeat(8)`; never render the raw tail). In `Verify.tsx`, after a successful verify response, if it carries the first key, `sessionStorage.setItem('mitos.firstKey', key)` before navigating to `/?uc=<useCase>`. If the Go verify response lacks the field, add `FirstKey string \`json:"firstKey,omitempty"\`` to the verify result/JSON (returned only on the first, provisioning verify), and assert it in `internal/saas/onboarding/http_test.go`. Never log the key.

- [ ] **Step 4: Run to verify it passes, then commit**

Run: `cd web/app && pnpm test -- firstrun/firstKey auth/Verify && pnpm build` (and if the Go response changed: `go test ./internal/saas/onboarding/ -run Verify -v && go build ./...`)
Expected: PASS, clean.

```bash
git add web/app/src/views/firstrun/firstKey.ts web/app/src/views/firstrun/firstKey.test.ts web/app/src/auth/Verify.tsx web/app/src/auth/Verify.test.tsx
# include the onboarding files if the Go response changed
git commit -s -m "feat(console): carry the one-time first key into the first-run"
```

---

### Task C5: a confetti celebration that honors reduced-motion

A small, dependency-free celebration component: a burst of brand-colored pieces on mount, disabled (renders nothing animated, just a calm "You are live" mark) under `prefers-reduced-motion: reduce`.

**Files:**
- Create: `web/app/src/ui/Celebrate.tsx`
- Test: `web/app/src/ui/Celebrate.test.tsx`

**Interfaces:**
- Produces: `<Celebrate active: boolean />` that, when `active`, renders an `aria-hidden` burst layer plus a visible, polite aria-live "You are live" status; under reduced-motion it renders only the status (no animated pieces). No third-party confetti dependency (use CSS keyframes gated behind a `matchMedia('(prefers-reduced-motion: reduce)')` check).

- [ ] **Step 1: Write the failing test**

`Celebrate.test.tsx`: with `active={true}` and `matchMedia` mocked to reduced-motion false, it renders the burst layer (`[data-testid="confetti-burst"]`) and a `role="status"` with "You are live"; with reduced-motion true it renders the status but NOT the burst layer; with `active={false}` it renders nothing. Mock `window.matchMedia`.

- [ ] **Step 2: Run to verify it fails**

Run: `cd web/app && pnpm test -- ui/Celebrate`
Expected: FAIL.

- [ ] **Step 3: Implement**

Create `Celebrate.tsx`: read reduced-motion via `window.matchMedia('(prefers-reduced-motion: reduce)').matches`. When `active`, render a `role="status"` `aria-live="polite"` "You are live" and, unless reduced-motion, an `aria-hidden` `div[data-testid="confetti-burst"]` with N absolutely-positioned pieces animated by a CSS keyframe (`transform` + `opacity`), colored with `var(--cyan)` / `var(--magenta)` tokens. Clean up nothing (pieces self-fade). No dashes.

- [ ] **Step 4: Run to verify it passes, then commit**

Run: `cd web/app && pnpm test -- ui/Celebrate && pnpm build`
Expected: PASS, clean.

```bash
git add web/app/src/ui/Celebrate.tsx web/app/src/ui/Celebrate.test.tsx
git commit -s -m "feat(console): reduced-motion-aware celebration component"
```

---

### Task C6: the elevated FirstRun (tabbed steps, copy-checks, waiting, celebrate)

Rebuild the FirstRun body as three tracked steps: (1) masked key export with copy that checks the step, (2) a runtime-tabbed snippet with copy that checks the step, (3) a live "waiting for your first call" state that flips to the celebration when `useFirstActivity` reports active. Keep `isFirstRun` and the intent-shaped `title`/`lede`/`watchFor`.

**Files:**
- Modify: `web/app/src/views/firstrun/FirstRun.tsx`
- Test: `web/app/src/views/firstrun/FirstRun.test.tsx`

**Interfaces:**
- Consumes: `getFirstRun`, `RUNTIMES`, `Runtime` (C1); `useFirstActivity` (C3); `peekFirstKey`, `takeFirstKey`, `maskKey` (C4); `Celebrate` (C5); `useBilling`, `fmtDollars` (existing).
- Produces: the elevated `<FirstRun uc?: string />` (same prop) and the unchanged `isFirstRun` export.

- [ ] **Step 1: Write the failing test**

Extend `FirstRun.test.tsx`. Mock `useBilling`, `useFirstActivity`, and `sessionStorage` (seed `mitos.firstKey`). Assert:
  - Step 1 shows a masked key (`mk_live_` prefix, dots, and the raw tail is NOT in the DOM) and an export line label; clicking "Copy" marks Step 1 done (a `[data-step="key"][data-done="true"]` or an accessible "Step 1 complete" state) and writes the clipboard.
  - Step 2 renders tabs from `RUNTIMES`; clicking the TypeScript tab shows the TS snippet; clicking "Copy" marks Step 2 done.
  - Step 3 shows "Waiting for your first call" while `useFirstActivity` returns `{active:false}`; when it returns `{active:true}`, the `Celebrate` status "You are live" appears.
  - With no seeded key, Step 1 shows a "Create an API key" link to `/keys` instead of the masked line.
  - No `uc` uses the default content title. No em/en dashes in rendered text.
Mock the clipboard (`navigator.clipboard.writeText`).

- [ ] **Step 2: Run to verify it fails**

Run: `cd web/app && pnpm test -- firstrun/FirstRun`
Expected: FAIL.

- [ ] **Step 3: Implement the elevated component**

Rewrite the FirstRun body (keep the styles block, extend it):
  - State: `keyDone`, `snippetDone` booleans; `runtime` (default `'python'`); `const key = peekFirstKey()`.
  - `const activity = useFirstActivity(true)`; `const active = activity.data?.active === true`.
  - Step 1 row: if `key`, render `export MITOS_API_KEY=` + `maskKey(key)` in a mono block with a Copy button whose handler copies `export MITOS_API_KEY=${key}` (the raw value, once), sets `keyDone`, and calls `takeFirstKey()` to clear it; announce via aria-live. If no `key`, render a `<Link to="/keys">Create an API key</Link>` line and treat the step as actionable (not auto-done).
  - Step 2 row: a tablist of `RUNTIMES` (real `role="tab"` buttons, arrow-key navigable, magenta focus ring), showing `content.snippets[runtime]`; a Copy button that copies the shown snippet and sets `snippetDone`.
  - Step 3 row: while `!active`, a calm "Waiting for your first call..." with a subtle pulse (reduced-motion static); render `<Celebrate active={active} />`; when `active`, also reveal next-step links (Open the fork tree `/forks`, View usage `/usage`, Add credits `/billing`).
  - Keep the billing credit/spend line and the intent `title`/`lede`. Each step shows its index and a done affordance (checkmark glyph or `aria` state). Tokens only, no dashes, 44px targets, focus rings.

- [ ] **Step 4: Run to verify it passes, build, then commit**

Run: `cd web/app && pnpm test -- firstrun/FirstRun && pnpm test && pnpm build`
Expected: PASS, clean (full suite green).

```bash
git add web/app/src/views/firstrun/FirstRun.tsx web/app/src/views/firstrun/FirstRun.test.tsx
git commit -s -m "feat(console): event-driven tabbed first-run with copy-checks and celebration"
```

---

### Task C7: confirm the Overview wiring and the active-org path

`Instruments.tsx` already renders `<FirstRun uc={ucFromSearch} />` when `isFirstRun` is true (from ws3). Confirm the elevated component still mounts there, reads `?uc` from the router search, and that an active org (`forks_served > 0`) hides the card and shows the normal panels.

**Files:**
- Modify (only if needed): `web/app/src/views/Instruments.tsx`
- Test: `web/app/src/views/Instruments.test.tsx`

**Interfaces:**
- Consumes: `FirstRun`, `isFirstRun` (Task C6); the router search for `?uc`.

- [ ] **Step 1: Write/confirm the failing test**

In `Instruments.test.tsx`, assert: when instruments report `forks_served: 0` and no sandboxes, the first-run heading renders; when `forks_served: 1`, the first-run heading does NOT render and the normal panels do. If the ws3 test already covers the first branch, add the active-org branch. Mock `useFirstActivity` so the mounted FirstRun does not error.

- [ ] **Step 2: Run to verify it fails (or passes if already wired)**

Run: `cd web/app && pnpm test -- views/Instruments`
Expected: FAIL on the new active-org assertion (or a mount error surfaced), else confirm green.

- [ ] **Step 3: Adjust if needed**

Only if the test fails: ensure `Instruments.tsx` reads `?uc` (router search or `window.location.search`), gates `<FirstRun/>` on `isFirstRun(instruments.data, sandboxes.data)`, and that the elevated FirstRun's `useFirstActivity` call does not run when the card is not mounted. No behavior change for an active org.

- [ ] **Step 4: Run tests + full build, then commit**

Run: `cd web/app && pnpm test && pnpm build`
Expected: PASS, clean.

```bash
git add web/app/src/views/Instruments.tsx web/app/src/views/Instruments.test.tsx
git commit -s -m "test(console): first-run shows for a new org and hides for an active one"
```

---

## Self-Review

**1. Spec coverage (this slice):** Phase 0 covers the spec's "Base and integration" (rebase the three prior branches, suite-gated). Workstream C covers the spec's elevated first-run: per-runtime tabs (C1), the server "first call" signal (C2, C3), the masked-key-shown-once step (C4), copy-checks-the-step and the tabbed snippet (C6), the celebration honoring reduced-motion (C5), and the Overview wiring / active-org path (C7). The spec's live-cost-ticking on the fork tree and the in-console fork button are explicitly fast-follows (not this slice). Workstreams A, B, D, E are separate slice plans.

**2. Placeholder scan:** each task names exact files, exact interfaces, real test assertions, and shows the snippet/hook/component code or its precise shape. The one deliberate "confirm against the real file" is the verify-response field name in C4, with a concrete fallback (add the field + test) if absent; that is a verification instruction, not a placeholder.

**3. Type consistency:** `FirstRunContent.snippets: Record<Runtime,string>` (C1) is consumed by FirstRun (C6); `RUNTIMES`/`Runtime` (C1) drive the C6 tablist; `useFirstActivity(enabled)` -> `{data:{active}}` (C3) matches the C2 `FirstActivityView{active}` JSON and the C6 `active` gate; `peekFirstKey`/`takeFirstKey`/`maskKey` (C4) are used exactly in C6; `Celebrate active` (C5) is the prop C6 passes. `isFirstRun` keeps its existing signature, so C7's Instruments wiring is unchanged in shape.

## Execution note

The live fork tree filling with real forks, and the `first-activity` signal flipping from a real SDK run, are verified on a deployed gated environment (SDK against a cluster), not locally; the locally verifiable surface is the tabbed steps, the copy-checks, the masked key, the polling state transition (mocked), and the celebration. Next slices: D (credits in Overview + spend cap + buy-credits Paddle top-up + durable stores), A (allowlist gate + website flip + `/waitlist`), B (canonical email + anti-abuse), E (per-view DX polish).
