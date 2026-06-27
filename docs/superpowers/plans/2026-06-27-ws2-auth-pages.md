# Workstream 2: native auth pages + connector wiring Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax.

**Goal:** Give Mitos a native, on-brand sign-in experience: pre-auth Login / Signup / Verify pages in the console SPA (rendered without the app shell), with "Continue with GitHub", "Continue with Google", and email, wired to the existing `/auth/*` (OIDC via Dex) and `/onboarding/*` endpoints, so login feels 100 percent native with no third-party login screen.

**Architecture:** The SPA detects the unauthenticated state (capabilities fetch returns 401) and mounts a small pre-auth router (`/login`, `/signup`, `/verify`) that renders standalone pages, instead of erroring. GitHub/Google buttons link to `/auth/login?connector=github|google`; the Go handler passes a `connector_id` hint to Dex so Dex skips its own chooser screen and goes straight to the provider, keeping the user on mitos.run pages. Email signup posts to `/onboarding/signup`; the verify page posts to `/onboarding/verify`. GitHub/Google handle both new and returning users via the existing `LoginManager` (auto-provision on first sign-in). Dex deployment is a separate follow-on plan.

**Tech Stack:** React 18 + TanStack Router + TanStack Query (web/app), Vitest + React Testing Library, `@mitos/brand` (Button, Card, Mark, Division, Fluorescence tokens), Go (`internal/saas/oidcauth`), go-oidc + x/oauth2.

## Global Constraints

- Never use em (U+2014) or en (U+2013) dashes anywhere (TSX, Go, CSS, comments, commit messages). ASCII hyphen only.
- Brand: Fluorescence tokens only (`var(--field)`, `--field-1`, `--ink`, `--ink-2`, `--magenta`, `--hairline`, `--r-md`, `--space-*`, `--step-*`). No second accent color. Headlines are concrete; CTAs imperative ("Continue with GitHub", "Send me a link"). Prove-don't-persuade voice. Buttons use `@mitos/brand` `Button`.
- Go: `fmt.Errorf("ctx: %w", err)`; gofmt + `go vet ./...` clean; secrets and tokens never logged.
- DCO: every commit `git commit -s` (required by CI). Conventional prefixes (feat, test, fix). Stage explicit paths only.
- Work in the ISOLATED WORKTREE /Users/jannesstubbemann/repos/mitos-run/mitos-ws2 (branch hosted-launch-journey). Run all commands there, NOT the main checkout.
- SPA commands run from `web/app`: `pnpm test` (vitest run), `pnpm build`. Go commands from the repo root of the worktree.
- Mirror existing patterns: `web/app/src/router.tsx`, `src/router.test.tsx`, `src/test/utils.tsx` (the `renderAt` helper), `src/api.ts` (the `get` fetch wrapper, `credentials: 'same-origin'`), and `internal/saas/oidcauth/handlers.go`.

---

### Task 1: Pre-auth routing (mount auth pages when unauthenticated)

Make the SPA render a standalone pre-auth router when the user has no session, instead of throwing on the 401 from `/console/capabilities`.

**Files:**
- Create: `web/app/src/auth/preauthRouter.tsx` (the pre-auth route tree: /login, /signup, /verify with placeholder components)
- Modify: `web/app/src/App.tsx` (branch: on capabilities 401 -> pre-auth router; else the existing authenticated router)
- Modify: `web/app/src/api.ts` (expose whether capabilities returned 401, e.g. a typed `UnauthorizedError`)
- Test: `web/app/src/auth/preauthRouter.test.tsx`

**Interfaces:**
- Consumes: `createConsoleRouter` (existing authenticated router), `useCapabilities`/`api.capabilities`, the `get` fetch wrapper.
- Produces: `createPreAuthRouter()` returning a TanStack Router with `/login`, `/signup`, `/verify` routes (no AppShell); an exported `UnauthorizedError` class; App.tsx selects the router by auth state.

- [ ] **Step 1: Write the failing test**

Create `web/app/src/auth/preauthRouter.test.tsx`. Mirror `src/router.test.tsx` (mock `fetch`). Mock `/console/capabilities` to return 401 and assert the login route renders (a "Continue with GitHub" affordance is present). Example:

```tsx
import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, screen } from '@testing-library/react'
import { createPreAuthRouter } from './preauthRouter'
import { RouterProvider } from '@tanstack/react-router'

describe('pre-auth router', () => {
  it('renders the login route with a GitHub affordance', async () => {
    const router = createPreAuthRouter('/login')
    render(<RouterProvider router={router} />)
    expect(await screen.findByText(/Continue with GitHub/i)).toBeInTheDocument()
  })
})
```

(If `createPreAuthRouter` needs an initial path for tests, accept an optional `initialPath` arg as `renderAt` does.)

- [ ] **Step 2: Run the test to verify it fails**

Run: `cd web/app && pnpm test -- preauthRouter`
Expected: FAIL (module not found / component undefined).

- [ ] **Step 3: Implement the pre-auth router with placeholder pages**

Create `web/app/src/auth/preauthRouter.tsx` with a `createRootRoute` (no AppShell, a minimal centered layout) and three child routes whose components are temporary placeholders (real pages land in Tasks 2-4). The `/login` placeholder must render text "Continue with GitHub" so the test passes. Mirror `router.tsx`'s `createRouter`/`createRootRoute`/`createRoute` usage and `defaultPreload: 'intent'`.

- [ ] **Step 4: Wire App.tsx to branch on auth state**

In `web/app/src/api.ts`, make the capabilities fetch distinguish 401: throw a typed `export class UnauthorizedError extends Error {}` when `r.status === 401`. In `App.tsx`, when the capabilities query errors with `UnauthorizedError`, render `<RouterProvider router={createPreAuthRouter()} />`; otherwise the existing authenticated router. Keep the loading state. Do not change the authenticated path behavior.

- [ ] **Step 5: Run the test and the suite**

Run: `cd web/app && pnpm test`
Expected: the new test passes; existing tests stay green. Run `pnpm build` to confirm the bundle compiles.

- [ ] **Step 6: Commit**

```bash
git add web/app/src/auth/preauthRouter.tsx web/app/src/auth/preauthRouter.test.tsx web/app/src/App.tsx web/app/src/api.ts
git commit -s -m "feat(console): mount pre-auth router (login/signup/verify) on unauthenticated state"
```

---

### Task 2: Login page

**Files:**
- Create: `web/app/src/auth/Login.tsx`
- Modify: `web/app/src/auth/preauthRouter.tsx` (use `<Login/>` for the `/login` route)
- Test: `web/app/src/auth/Login.test.tsx`

**Interfaces:**
- Consumes: `@mitos/brand` `Button`, `Card`, `Mark`; the Fluorescence tokens.
- Produces: a `<Login/>` page with GitHub and Google buttons that navigate to `/auth/login?connector=github` and `?connector=google`, an email field that navigates to `/signup`, and a `?next=` passthrough.

- [ ] **Step 1: Write the failing test**

Create `Login.test.tsx`: render `<Login/>`, assert "Continue with GitHub" and "Continue with Google" anchors point to `/auth/login?connector=github` and `?connector=google` (read `href`), and that an email input + a "Sign up with email" affordance exist.

```tsx
import { render, screen } from '@testing-library/react'
import { Login } from './Login'

it('offers GitHub and Google with connector hints', () => {
  render(<Login />)
  expect(screen.getByRole('link', { name: /Continue with GitHub/i })).toHaveAttribute('href', '/auth/login?connector=github')
  expect(screen.getByRole('link', { name: /Continue with Google/i })).toHaveAttribute('href', '/auth/login?connector=google')
})
```

- [ ] **Step 2: Run to verify it fails**

Run: `cd web/app && pnpm test -- Login`
Expected: FAIL (no Login component).

- [ ] **Step 3: Implement the Login page**

Create `Login.tsx`: a centered `Card` on the `--field` canvas with the `Mark` logo, a concise headline ("A computer for every agent" or "Sign in to Mitos"; keep it concrete and on-brand), the two provider anchors (plain `<a href>` so it is a full navigation to the Go `/auth/login` endpoint, NOT a client route), and below a hairline divider an email input plus a `Button` that routes to `/signup` (carry the email as a query param if present). Honor a `?next=` param by appending it to the provider hrefs (`/auth/login?connector=github&next=...`). Style with tokens only; provider buttons use `Button variant="ghost"` with a small inline provider glyph (inline SVG, no external asset, aria-hidden). No em/en dashes.

- [ ] **Step 4: Run the test and suite, then commit**

Run: `cd web/app && pnpm test && pnpm build`
Expected: PASS, build clean.

```bash
git add web/app/src/auth/Login.tsx web/app/src/auth/Login.test.tsx web/app/src/auth/preauthRouter.tsx
git commit -s -m "feat(console): native Login page with GitHub/Google connector hints and email"
```

---

### Task 3: Signup page

**Files:**
- Create: `web/app/src/auth/Signup.tsx`
- Modify: `web/app/src/auth/preauthRouter.tsx`
- Test: `web/app/src/auth/Signup.test.tsx`

**Interfaces:**
- Consumes: `@mitos/brand` `Button`, `Card`; a POST to `/onboarding/signup` with `{ email }` (use a `post` helper; if `api.ts` lacks one, add `post<T>(path, body)` mirroring `get`, `credentials: 'same-origin'`, JSON).
- Produces: a `<Signup/>` page that submits the email and shows a "check your email" confirmation state on the 202 response.

- [ ] **Step 1: Write the failing test**

`Signup.test.tsx`: mock `fetch` for `POST /onboarding/signup` to resolve 202; render `<Signup/>`, type an email, submit, assert a confirmation message like "check your email" appears and that fetch was called with the email. Also assert the GitHub/Google options are present (signup should also offer social, since social handles new users too).

- [ ] **Step 2: Run to verify it fails**

Run: `cd web/app && pnpm test -- Signup` (FAIL: no component).

- [ ] **Step 3: Implement Signup**

Create `Signup.tsx`: same provider buttons as Login (GitHub/Google via `/auth/login?connector=...`, because social signs up new users too), plus an email field and a `Button type="submit"` "Send me a sign-in link". On submit, POST `/onboarding/signup` with `{ email }`; on success show the confirmation state ("Check your email for a link to finish signing up. No card needed; you start with free credit."). Handle the error state (400) with a non-leaky message. If a Signup-from-Login email was passed via query, prefill it. Tokens-only styling. No dashes.

- [ ] **Step 4: Run and commit**

Run: `cd web/app && pnpm test && pnpm build` (PASS).

```bash
git add web/app/src/auth/Signup.tsx web/app/src/auth/Signup.test.tsx web/app/src/auth/preauthRouter.tsx web/app/src/api.ts
git commit -s -m "feat(console): native Signup page wired to /onboarding/signup"
```

---

### Task 4: Verify page

**Files:**
- Create: `web/app/src/auth/Verify.tsx`
- Modify: `web/app/src/auth/preauthRouter.tsx`
- Test: `web/app/src/auth/Verify.test.tsx`

**Interfaces:**
- Consumes: the `?token=` query param; a POST to `/onboarding/verify` with `{ token }` returning `{ accountId, orgId, email, alreadyDone, apiKey?, apiKeyId? }`.
- Produces: a `<Verify/>` page that exchanges the token, shows the first API key exactly once with a copy affordance, and a "Continue to console" action.

- [ ] **Step 1: Write the failing test**

`Verify.test.tsx`: mock `POST /onboarding/verify` to return `{ accountId:'a', orgId:'o', email:'e@x.com', alreadyDone:false, apiKey:'mitos_live_xxx', apiKeyId:'k1' }`; render `<Verify/>` at `/verify?token=t`; assert the page shows the key `mitos_live_xxx` and a "Continue to console" link. Add a second case: `alreadyDone:true` with no `apiKey` shows a "already verified, continue" state without a key.

- [ ] **Step 2: Run to verify it fails**

Run: `cd web/app && pnpm test -- Verify` (FAIL).

- [ ] **Step 3: Implement Verify**

Create `Verify.tsx`: on mount, read `?token`; POST `/onboarding/verify` with `{ token }`. States: loading; success-with-key (show `apiKey` once in a `Card` with a copy button and a clear "save this now, it is shown once" note, plus the install snippet `pip install mitos-run`); success-already-done (no key, "you are verified" + continue); error (invalid/expired link, with a link back to `/signup`). "Continue to console" is a full navigation to `/` (so the authenticated router loads). Tokens-only. No dashes.

Note on session: the verify endpoint provisions the account but does not set a session cookie today. For launch, after a successful verify the user clicks "Continue to console"; if `/` still shows the pre-auth router (no session), they sign in via GitHub/Google or the email link. Wiring verify to also issue a session cookie is a documented follow-up (it needs the onboarding handler to mint a session); this plan does not change the backend session behavior.

- [ ] **Step 4: Run and commit**

Run: `cd web/app && pnpm test && pnpm build` (PASS).

```bash
git add web/app/src/auth/Verify.tsx web/app/src/auth/Verify.test.tsx web/app/src/auth/preauthRouter.tsx
git commit -s -m "feat(console): native Verify page showing the first API key once"
```

---

### Task 5: Connector hint in the Go OIDC login handler

Pass a `connector_id` hint to Dex so "Continue with GitHub/Google" skips Dex's own chooser screen.

**Files:**
- Modify: `internal/saas/oidcauth/handlers.go` (Login reads `?connector=` and adds it as an auth-code option)
- Modify: `internal/saas/oidcauth/verifier.go` (the real `Exchanger.AuthCodeURL` accepts extra `oauth2.AuthCodeOption`s, or add a method that injects `connector_id`)
- Test: `internal/saas/oidcauth/handlers_test.go` (assert the redirect URL carries `connector_id` when `?connector=` is set, and omits it otherwise)

**Interfaces:**
- Consumes: the existing `Exchanger` interface and `Handlers.Login`.
- Produces: `Login` honoring an allowlisted `connector` query value (`github`, `google`) by adding `oauth2.SetAuthURLParam("connector_id", connector)` to the `AuthCodeURL` call. Unknown/empty connector -> no hint (Dex shows its chooser).

- [ ] **Step 1: Write the failing test**

In `handlers_test.go`, add a test that calls `Login` with `?connector=github`, captures the 302 `Location`, parses it, and asserts the query contains `connector_id=github`. Add a second case with no `connector` param asserting `connector_id` is absent. Read the existing handler test setup (fake `Exchanger`) and mirror it; the fake `AuthCodeURL` must reflect the passed options so the test can observe the param (extend the fake if needed).

- [ ] **Step 2: Run to verify it fails**

Run (from the worktree root): `go test ./internal/saas/oidcauth/ -run TestLoginConnector -v`
Expected: FAIL (connector not yet honored).

- [ ] **Step 3: Implement**

In `handlers.go` `Login`: read `connector := r.URL.Query().Get("connector")`; allowlist it to `{"github","google"}` (ignore anything else, no error); when set, pass `oauth2.SetAuthURLParam("connector_id", connector)` into the `Exchanger.AuthCodeURL` call. This requires `AuthCodeURL` to accept `...oauth2.AuthCodeOption`; update the `Exchanger` interface and the real implementation in `verifier.go` accordingly (the real `oauth2.Config.AuthCodeURL(state, opts...)` already takes options). Keep the existing `state` CSRF behavior unchanged. Also thread an allowlisted `next` param into `RedirectAfterLogin` if present (optional; if it complicates the CSRF/state handling, leave `next` for a follow-up and note it).

- [ ] **Step 4: Run the test and vet, then commit**

Run: `go test ./internal/saas/oidcauth/ -v && go vet ./internal/saas/oidcauth/ && go build ./...`
Expected: PASS, clean.

```bash
git add internal/saas/oidcauth/handlers.go internal/saas/oidcauth/verifier.go internal/saas/oidcauth/handlers_test.go
git commit -s -m "feat(oidcauth): pass connector_id hint so GitHub/Google skip the Dex chooser"
```

---

## Self-Review

**1. Spec coverage (WS2 Component 2, auth pages):** native Login/Signup/Verify pages on-brand (Tasks 2-4), pre-auth routing so unauthenticated users see them instead of a 401 (Task 1), GitHub/Google via the existing `/auth/*` with connector hints so no Dex chooser is seen (Tasks 2, 5), email signup via `/onboarding/signup` (Task 3), first key shown once (Task 4). Dex DEPLOYMENT and wiring the console to Dex's issuer are a separate follow-on plan (noted). Returning-user email-only login and verify-sets-session are documented follow-ups (Task 4 note); GitHub/Google covers new and returning uniformly at launch.

**2. Placeholder scan:** Page components are specified by exact behavior, brand usage, and the endpoints/hrefs they hit, with the load-bearing test code and the connector-wiring Go change given concretely. The pages follow the existing `router.test.tsx` / `renderAt` / `api.get` patterns the implementer can read; this is intentional (UI mirroring existing patterns) rather than a placeholder.

**3. Type/contract consistency:** `/auth/login?connector=github|google` (Task 2 hrefs) matches the connector allowlist honored in Task 5. `/onboarding/signup` (Task 3) and `/onboarding/verify` returning `{accountId,orgId,email,alreadyDone,apiKey?,apiKeyId?}` (Task 4) match the onboarding HTTP handler shapes. `UnauthorizedError` (Task 1) is what App.tsx branches on. The `post` helper added in Task 3 is reused by Task 4.

Note for the executor: Tasks 1-4 are frontend (vitest, `cd web/app`), Task 5 is Go. The full social sign-in round trip cannot be verified without a running Dex (follow-on plan); these tasks verify the UI, the hrefs, the onboarding calls, and the connector-hint redirect param, which is the locally verifiable surface.
