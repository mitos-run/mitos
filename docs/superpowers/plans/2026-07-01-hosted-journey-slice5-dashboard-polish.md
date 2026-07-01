# Hosted Journey Slice E: Dashboard UX/DX polish Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Bring the mitos console dashboard to a uniformly high craft bar by removing the one off-pattern dead view and closing the accessibility-test coverage gap on the three views a newly signed-up user actually traverses.

**Architecture:** Slice E is the final craft pass of the hosted-journey completion (design spec `docs/superpowers/specs/2026-07-01-hosted-journey-completion-design.md`, section E). The heavy lifting already landed: the `feat/console-ux-polish` shell (PageHeader, TopBar, AccountMenu, CommandPalette, EmptyState, Skeleton, Tabs, Toast) was folded in during step-0 integration, and slices C/D polished the first-run and billing surfaces. An audit of the routed views found the bar is already consistent: every routed data table is wrapped for horizontal scroll on mobile AND carries an `aria-label` with `scope="col"` headers, PageHeader adoption is complete, and there are zero em/en dashes in view copy. The only two real gaps remain: (1) `web/app/src/views/Sandboxes.tsx` is dead code (unrouted, superseded by `sandboxes/SandboxList.tsx` + `forktree/ForkTree.tsx`) and it is the sole view still off the shell bar; (2) the journey views Overview (`Instruments`), `Keys`, and `Billing` lack the `.a11y.test.tsx` axe coverage that their sibling views (Audit, Retention, Roles, Settings, Members, Projects) already have. This slice closes both. No scope creep beyond the journey.

**Tech Stack:** React 18 + TypeScript + Vite, TanStack Router + Query, Vitest + React Testing Library + `vitest-axe` (^0.1.0), pnpm. All console SPA code under `web/app/`.

## Global Constraints

- Work in the git worktree `/Users/jannesstubbemann/repos/mitos-run/mitos-journey` on branch `feat/hosted-journey-finish`. All commits `git commit -s` (DCO), conventional-commit messages.
- NO em dashes and NO en dashes anywhere: code, comments, tests, copy, commit messages. Use colons, periods, parentheses, or commas.
- Brand voice in any user-facing copy: plain, accessible, confident; no MBA jargon.
- Run all SPA commands from `web/app/`: `pnpm test` (Vitest) and `pnpm build` (tsc + vite) must both stay clean. `pnpm test <file>` runs a single test file.
- Do NOT restructure or rename existing components; follow the established patterns (functional views, `renderAt(path, caps)` test helper, `vitest-axe` matchers). Leave each file tidier than found; no unrelated refactoring.
- Mirror existing code: the reference a11y test is `web/app/src/views/Settings.a11y.test.tsx`; the reference polished list view is `web/app/src/views/sandboxes/SandboxList.tsx`.

---

### Task E1: Remove the dead `Sandboxes.tsx` view

**Why:** `web/app/src/views/Sandboxes.tsx` is not imported by the router (`web/app/src/nav/routes.tsx` routes `/sandboxes` to `sandboxes/SandboxList.tsx` and `/forks` to `forktree/ForkTree.tsx`). It is the only view still using raw `useEffect`/`useState` fetching, with no `PageHeader`, an ad-hoc `setErr(String(e))` error surface, and no loading or empty state. It has no test file. It drags the console off its otherwise-uniform bar and is pure dead weight. Deleting it is the correct "leave it tidier" fix; there is nothing to salvage (SandboxList + ForkTree already deliver the live-sandboxes list and the fork-tree centerpiece).

**Files:**
- Delete: `web/app/src/views/Sandboxes.tsx`

**Interfaces:**
- Consumes: nothing (leaf file).
- Produces: nothing (removing an unexported-from-routes module).

- [ ] **Step 1: Prove the file is unreferenced**

Run (from `web/app`):
```bash
grep -rn "views/Sandboxes'\|from './Sandboxes'\|{ Sandboxes }" src --include='*.ts' --include='*.tsx' | grep -v "SandboxList\|SandboxDetail\|sandboxes/\|data/sandboxes\|useSandboxes"
```
Expected: NO output (the `Sandboxes.tsx` module has zero importers). The routed views are `SandboxList` and `ForkTree`; the `useSandboxes` hook and the `sandboxes/` directory are unrelated. If this grep prints any line, STOP: the file is live, do not delete it, report the reference back.

- [ ] **Step 2: Delete the dead file**

Run:
```bash
git rm web/app/src/views/Sandboxes.tsx
```

- [ ] **Step 3: Verify the build is clean (typecheck catches any dangling reference)**

Run (from `web/app`):
```bash
pnpm build
```
Expected: build succeeds with no TypeScript error about a missing `Sandboxes` module.

- [ ] **Step 4: Verify the full test suite is green**

Run (from `web/app`):
```bash
pnpm test
```
Expected: all tests pass (no test imported the deleted file). Note: `App.test.tsx`, `router.test.tsx`, and the AppShell tests reference the string "Sandboxes" as the nav LABEL for the routed `SandboxList`, not the deleted module; they must still pass.

- [ ] **Step 5: Commit**

```bash
git add -A
git commit -s -m "refactor(console): remove dead Sandboxes view superseded by SandboxList and ForkTree"
```

---

### Task E2: Add axe accessibility coverage for the three journey views

**Why:** The signup-to-first-value journey lands a user on Overview (`Instruments`, route `/`), and the loved dashboard promise is exercised most on `Keys` (route `/keys`, where they copy their first API key) and `Billing` (route `/billing`, where they see credit and buy more). Their sibling views already have `.a11y.test.tsx` axe coverage (Audit, Retention, Roles, Settings, Members, Projects); these three do not. Add matching coverage so keyboard/focus/label quality on the journey surfaces is verified and locked in. If axe surfaces a real violation, fix the view minimally (add a missing `aria-label`, `scope`, or label association) rather than suppressing the rule.

**Files:**
- Create: `web/app/src/views/Instruments.a11y.test.tsx`
- Create: `web/app/src/views/Keys.a11y.test.tsx`
- Create: `web/app/src/views/Billing.a11y.test.tsx`
- Reference (read, do not modify): `web/app/src/views/Settings.a11y.test.tsx` (vitest-axe boilerplate), `web/app/src/views/Instruments.test.tsx`, `web/app/src/views/Keys.test.tsx`, `web/app/src/views/Billing.test.tsx` (each already contains the correct `fetch` mock setup for its view).
- Possibly modify (only if axe flags a violation): the corresponding view file under `web/app/src/views/`.

**Interfaces:**
- Consumes: `renderAt(path, caps)` from `web/app/src/test/utils.tsx` (renders the app at a route inside Query + Toast providers); `axe` from `vitest-axe` and matchers from `vitest-axe/matchers`; the `Capabilities` and view data types from `web/app/src/api.ts`.
- Produces: three new test files; no exported symbols.

**Reference pattern (from `Settings.a11y.test.tsx`):**
```tsx
import { describe, it, expect, vi, beforeEach } from 'vitest'
import { axe } from 'vitest-axe'
import * as matchers from 'vitest-axe/matchers'
import { renderAt } from '../test/utils'
import type { Capabilities } from '../api'

expect.extend(matchers)
// ... beforeEach mocks globalThis.fetch per endpoint, returning a catch-all {} 200 for anything unmocked ...
```
The routed data endpoints are: Overview reads `/console/instruments`, `/console/sandboxes`, `/console/billing`, `/console/audit`; Keys reads `/console/keys`; Billing reads `/console/billing`. Reuse the exact mock payloads already written in each view's sibling `<View>.test.tsx` so the populated DOM (tables, buttons, forms) is what axe audits, and keep a catch-all `{}` 200 response for any endpoint not explicitly mocked (as `Settings.a11y.test.tsx` does). Every view also needs `/console/capabilities` mocked to return `caps`.

**Capabilities per view** (from `web/app/src/nav/routes.tsx` route guards):
- Overview (`/`) and Keys (`/keys`): always routed; a baseline caps object works.
- Billing (`/billing`): guarded by `when: (c) => c.billing`, so its caps MUST set `billing: true` (otherwise the route does not render).

Use this baseline caps object (copy the exact shape from `Settings.a11y.test.tsx`, adjusting `billing`):
```tsx
const caps: Capabilities = {
  edition: 'community', billing: true, signup: false, teams: true, idp: 'oidc',
  orgSwitcher: false, secrets: { providers: ['kube'] }, proof: true, ownership: 'self-hosted',
}
```

- [ ] **Step 1: Write the Keys a11y test (start with the simplest view)**

Create `web/app/src/views/Keys.a11y.test.tsx`. Copy the vitest-axe boilerplate from `Settings.a11y.test.tsx`. Copy the `fetch` mock `beforeEach` from `Keys.test.tsx` (it already mocks `/console/keys` with a realistic key list and `/console/capabilities`); add a catch-all `{}` 200 for any other URL. Body:
```tsx
describe('Keys accessibility', () => {
  it('has no axe violations', async () => {
    const { container } = await renderAt('/keys', caps)
    // Let the query resolve so axe audits the populated table, not the skeleton.
    await screen.findByRole('heading', { name: /api keys/i })
    expect(await axe(container)).toHaveNoViolations()
  })
})
```
(Import `screen` from `@testing-library/react`. If the heading text differs, use the actual `PageHeader` title rendered by `Keys.tsx`.)

- [ ] **Step 2: Run the Keys a11y test**

Run (from `web/app`):
```bash
pnpm test src/views/Keys.a11y.test.tsx
```
Expected: PASS. If axe reports a violation, read the violation, fix `Keys.tsx` minimally (for example add a missing `aria-label` to an icon-only button or associate a form `<label>` with its input via `htmlFor`/`id`), and re-run until green. Do not disable the axe rule.

- [ ] **Step 3: Write and run the Billing a11y test**

Create `web/app/src/views/Billing.a11y.test.tsx` the same way, reusing the `fetch` mock from `Billing.test.tsx` (mocks `/console/billing`) with `billing: true` caps. Render at `/billing`, await the Billing PageHeading, assert `toHaveNoViolations()`. Run:
```bash
pnpm test src/views/Billing.a11y.test.tsx
```
Expected: PASS (fix any surfaced violation in `Billing.tsx` minimally, as in Step 2).

- [ ] **Step 4: Write and run the Overview (Instruments) a11y test**

Create `web/app/src/views/Instruments.a11y.test.tsx`, reusing the `fetch` mock from `Instruments.test.tsx` (mocks `/console/instruments`, `/console/sandboxes`, `/console/billing`, `/console/audit`) plus the catch-all. Render at `/` (the Overview route renders `<Instruments />`), await a stable element the populated view renders, assert `toHaveNoViolations()`. Run:
```bash
pnpm test src/views/Instruments.a11y.test.tsx
```
Expected: PASS (fix any surfaced violation in `Instruments.tsx` minimally).

- [ ] **Step 5: Run the full suite and build**

Run (from `web/app`):
```bash
pnpm test && pnpm build
```
Expected: all tests pass; build clean.

- [ ] **Step 6: Commit**

```bash
git add web/app/src/views/Instruments.a11y.test.tsx web/app/src/views/Keys.a11y.test.tsx web/app/src/views/Billing.a11y.test.tsx
# plus any view file you had to fix for an axe violation
git commit -s -m "test(console): axe accessibility coverage for Overview, Keys, and Billing journey views"
```

---

## Self-Review

**1. Spec coverage:** Design spec section E asks for "consistent PageHeader usage, empty and loading states that read as a waiting canvas, keyboard and focus order, mobile responsiveness, copy in brand voice" with "each view leaves tidier than found; no scope creep." The audit found PageHeader/empty/loading/mobile-table/aria/dash-free already uniform across ALL routed views, so the only outstanding work is the dead `Sandboxes.tsx` (E1, tidier + consistency) and keyboard/focus/label verification via axe on the journey views that lacked it (E2). The `feat/console-ux-polish` fold-in named in the spec was completed at step 0 (shell components present). No other section-E gap remains open; covered.

**2. Placeholder scan:** No TBD/TODO/"handle edge cases". E2 intentionally points the implementer at existing sibling `<View>.test.tsx` files for the exact mock payloads (DRY: those mocks already exist and are correct) rather than duplicating large fixtures here; the test body code is given in full. E1 is a deletion with an exact guard grep.

**3. Type consistency:** `renderAt(path, caps)` signature and the `Capabilities` shape are copied verbatim from the existing `Settings.a11y.test.tsx` / `test/utils.tsx`. Route paths (`/`, `/keys`, `/billing`) and endpoint URLs (`/console/instruments|sandboxes|billing|audit|keys|capabilities`) match `nav/routes.tsx` and `api.ts`. Consistent.
