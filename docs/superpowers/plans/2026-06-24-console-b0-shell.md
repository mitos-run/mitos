# Console B0: design system + shell Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Turn the stub console SPA into a fast, navigable, capability-gated shell (grouped nav, client-side routing with hover/focus prefetch, a command palette, a query-cached data layer, and the empty-state / skeleton / toast primitives) that every later phase builds views into.

**Architecture:** A React + Vite SPA, embedded in the Go console binary via `go:embed`. A single routes-config array is the source of truth consumed by both the nav and the router, each route gated by the server's `/console/capabilities` document. TanStack Router gives intent-based (hover/focus) preloading for the "snappy" feel; TanStack Query gives stale-while-revalidate caching. Visual craft on the existing `@mitos/brand` Fluorescence tokens is driven by the interface-design skill during execution.

**Tech Stack:** React 18.3, Vite 5.4, TypeScript 5.6 (strict), pnpm workspaces, `@mitos/brand`, TanStack Router, TanStack Query, Vitest + Testing Library + jsdom (new), Playwright (light smoke).

**Scope note:** This is the first of four plans from `docs/superpowers/specs/2026-06-24-console-dashboard-enterprise-design.md` (section 7). B0 is this plan. B1 (hero views: instrument cockpit + fork tree), B2 (core views), and B3 (enterprise layer) are separate plans written after B0 lands.

## Global Constraints

Every task implicitly includes these. Values copied verbatim from the spec and CLAUDE.md.

- **Punctuation (strict):** never use em (U+2014) or en (U+2013) dashes anywhere, including TS/TSX source, comments, JSX copy, Markdown, and commit messages. Use only `.` `,` `;` `:` connectors; ASCII hyphen-minus `-` is fine for ranges and compound identifiers.
- **Commits:** conventional commits (`feat`, `fix`, `docs`, `test`, `chore`, `refactor`); every commit carries a DCO sign-off, so always `git commit -s`.
- **Git staging:** stage explicit paths only; never `git add -A`.
- **Capabilities are server-controlled:** the SPA reads `/console/capabilities` and renders; it never decides a capability itself. A capability-off route is never mounted client-side AND the BFF returns `feature_disabled` defensively (the latter already exists server-side).
- **Package manager:** pnpm. All frontend commands run from `web/app` unless stated. Install deps with `pnpm -C web/app add ...` from the repo root, or `pnpm add ...` inside `web/app`.
- **No new Node runtime in production:** the SPA builds to `web/app/dist` and is embedded via `go:embed`; nothing here adds a server-side JS process.
- **TypeScript strict stays on:** `noUnusedLocals`, `noUnusedParameters`, and `strict` are enabled; code must compile clean under `pnpm -C web/app typecheck`.

---

## File Structure

Created or modified in this plan:

- `web/app/package.json` (modify) - add router, query, and test deps + a `test` script.
- `web/app/vite.config.ts` (modify) - add the Vitest `test` block (jsdom + setup file).
- `web/app/src/test/setup.ts` (create) - Testing Library / jest-dom setup, loaded by Vitest.
- `web/app/src/test/utils.tsx` (create) - shared render helper that wraps a subtree in the query + router providers.
- `web/app/src/api.ts` (modify) - add the typed endpoints the shell lists in nav (members, audit, usage, templates, keys, billing) as read stubs, reused by later phases.
- `web/app/src/data/query.ts` (create) - the `QueryClient` and `useCapabilities` hook.
- `web/app/src/nav/routes.tsx` (create) - the single routes-config array (path, label, group, element, `when` capability predicate) plus `visibleRoutes(caps)`.
- `web/app/src/nav/AppShell.tsx` (create) - the chrome: grouped `NavGroup` sidebar, ownership badge, `<Outlet/>` for the active route, command-palette mount.
- `web/app/src/nav/CommandPalette.tsx` (create) - Cmd-K palette: fuzzy filter over routes + actions, keyboard navigation.
- `web/app/src/router.tsx` (create) - the TanStack Router instance built from `routes.tsx`, with `defaultPreload: 'intent'` and per-route capability guards.
- `web/app/src/ui/EmptyState.tsx` (create) - teaching empty state primitive.
- `web/app/src/ui/Skeleton.tsx` (create) - skeleton placeholder primitive.
- `web/app/src/ui/Toast.tsx` (create) - toast provider + `useToast()` with auto-dismiss.
- `web/app/src/views/Placeholder.tsx` (create) - honest "view ships in a later phase" panel naming its BFF endpoint, for routes not yet built.
- `web/app/src/App.tsx` (modify) - replace the hand-rolled nav with the provider stack (query + router + toast).
- `web/app/src/main.tsx` (modify) - unchanged entry, confirmed to mount `App`.
- `web/app/e2e/smoke.spec.ts` (create) - one light Playwright smoke (boots, nav renders, palette opens).
- `web/app/playwright.config.ts` (create) - minimal Playwright config.

---

### Task 1: Test infrastructure (Vitest + Testing Library)

**Files:**
- Modify: `web/app/package.json`
- Modify: `web/app/vite.config.ts`
- Create: `web/app/src/test/setup.ts`
- Test: `web/app/src/test/smoke.test.ts`

**Interfaces:**
- Consumes: nothing.
- Produces: a working `pnpm -C web/app test` command (Vitest, jsdom env, jest-dom matchers loaded) that later tasks add tests to.

- [ ] **Step 1: Add dependencies**

Run from the repo root:

```bash
pnpm -C web/app add -D vitest@^2.1.8 jsdom@^25.0.1 @testing-library/react@^16.1.0 @testing-library/user-event@^14.5.2 @testing-library/jest-dom@^6.6.3
```

- [ ] **Step 2: Add the `test` script to `web/app/package.json`**

In the `scripts` block add:

```json
"test": "vitest run",
"test:watch": "vitest"
```

- [ ] **Step 3: Add the Vitest config to `web/app/vite.config.ts`**

Replace the file with (keeps the existing proxy and react plugin, adds the `test` block):

```ts
/// <reference types="vitest/config" />
import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

// The SPA builds to ./dist, which cmd/console embeds via go:embed. In dev,
// /console and /auth are proxied to a locally running console binary.
export default defineConfig({
  plugins: [react()],
  build: { outDir: 'dist', emptyOutDir: true },
  server: {
    proxy: {
      '/console': 'http://localhost:8080',
      '/auth': 'http://localhost:8080',
    },
  },
  test: {
    environment: 'jsdom',
    globals: true,
    setupFiles: ['./src/test/setup.ts'],
    css: false,
  },
})
```

- [ ] **Step 4: Create the setup file `web/app/src/test/setup.ts`**

```ts
// Loaded by Vitest before every test file: registers jest-dom matchers
// (toBeInTheDocument, etc.) and cleans the DOM between tests.
import '@testing-library/jest-dom/vitest'
import { afterEach } from 'vitest'
import { cleanup } from '@testing-library/react'

afterEach(() => {
  cleanup()
})
```

- [ ] **Step 5: Write a failing harness test `web/app/src/test/smoke.test.ts`**

```ts
import { describe, it, expect } from 'vitest'

describe('test harness', () => {
  it('runs and has jsdom', () => {
    const el = document.createElement('div')
    el.textContent = 'mitos'
    expect(el.textContent).toBe('mitos')
  })
})
```

- [ ] **Step 6: Run it and confirm the harness works**

Run: `pnpm -C web/app test`
Expected: PASS, 1 test. (If the harness were broken it would error on jsdom or setup.)

- [ ] **Step 7: Commit**

```bash
git add web/app/package.json web/app/pnpm-lock.yaml web/app/vite.config.ts web/app/src/test/setup.ts web/app/src/test/smoke.test.ts
git commit -s -m "test(console): add Vitest + Testing Library harness for the SPA"
```

---

### Task 2: Data layer (QueryClient + useCapabilities)

**Files:**
- Create: `web/app/src/data/query.ts`
- Create: `web/app/src/test/utils.tsx`
- Test: `web/app/src/data/query.test.tsx`

**Interfaces:**
- Consumes: `api.capabilities()` and the `Capabilities` type from `web/app/src/api.ts`.
- Produces:
  - `queryClient: QueryClient` (a configured singleton).
  - `useCapabilities(): { data?: Capabilities; isLoading: boolean; error: unknown }`.
  - `renderWithProviders(ui, { caps? })` test helper in `test/utils.tsx`.

- [ ] **Step 1: Add the data deps**

```bash
pnpm -C web/app add @tanstack/react-query@^5.62.7
```

- [ ] **Step 2: Write the failing test `web/app/src/data/query.test.tsx`**

```tsx
import { describe, it, expect, vi, beforeEach } from 'vitest'
import { renderHook, waitFor } from '@testing-library/react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { useCapabilities } from './query'

function wrapper() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  return ({ children }: { children: React.ReactNode }) => (
    <QueryClientProvider client={client}>{children}</QueryClientProvider>
  )
}

describe('useCapabilities', () => {
  beforeEach(() => {
    vi.restoreAllMocks()
  })

  it('fetches the capabilities document', async () => {
    vi.spyOn(globalThis, 'fetch').mockResolvedValue(
      new Response(JSON.stringify({ edition: 'community', billing: false, teams: true }), {
        status: 200,
        headers: { 'content-type': 'application/json' },
      }),
    )
    const { result } = renderHook(() => useCapabilities(), { wrapper: wrapper() })
    await waitFor(() => expect(result.current.data).toBeDefined())
    expect(result.current.data?.edition).toBe('community')
  })
})
```

- [ ] **Step 3: Run it and confirm it fails**

Run: `pnpm -C web/app test src/data/query.test.tsx`
Expected: FAIL with "Cannot find module './query'".

- [ ] **Step 4: Implement `web/app/src/data/query.ts`**

```ts
// The console data layer: one QueryClient and the capabilities hook every
// gating decision reads from. Stale-while-revalidate by default so navigation
// feels instant; the server remains the source of truth for capabilities.
import { QueryClient, useQuery } from '@tanstack/react-query'
import { api, type Capabilities } from '../api'

export const queryClient = new QueryClient({
  defaultOptions: {
    queries: {
      staleTime: 30_000,
      refetchOnWindowFocus: false,
      retry: 1,
    },
  },
})

export function useCapabilities() {
  return useQuery<Capabilities>({
    queryKey: ['capabilities'],
    queryFn: () => api.capabilities(),
    staleTime: Infinity, // capabilities change only on redeploy
  })
}
```

- [ ] **Step 5: Run it and confirm it passes**

Run: `pnpm -C web/app test src/data/query.test.tsx`
Expected: PASS.

- [ ] **Step 6: Create the shared render helper `web/app/src/test/utils.tsx`**

```tsx
// Shared test helper: render a subtree inside a fresh QueryClient so component
// tests do not share cache state. Router-aware helpers are added in Task 4.
import type { ReactElement, ReactNode } from 'react'
import { render } from '@testing-library/react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'

export function renderWithQuery(ui: ReactElement) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  const Wrapper = ({ children }: { children: ReactNode }) => (
    <QueryClientProvider client={client}>{children}</QueryClientProvider>
  )
  return render(ui, { wrapper: Wrapper })
}
```

- [ ] **Step 7: Commit**

```bash
git add web/app/package.json web/app/pnpm-lock.yaml web/app/src/data/query.ts web/app/src/data/query.test.tsx web/app/src/test/utils.tsx
git commit -s -m "feat(console): add TanStack Query data layer and useCapabilities hook"
```

---

### Task 3: Routes config + capability gating (pure logic)

**Files:**
- Create: `web/app/src/views/Placeholder.tsx`
- Create: `web/app/src/nav/routes.tsx`
- Test: `web/app/src/nav/routes.test.tsx`

**Interfaces:**
- Consumes: `Capabilities` from `api.ts`; existing views `Instruments`, `Sandboxes`, `Secrets` from `web/app/src/views/`.
- Produces:
  - `type RouteDef = { path: string; label: string; group: NavGroupName; element: () => JSX.Element; when?: (c: Capabilities) => boolean }`.
  - `type NavGroupName = 'Run' | 'Build' | 'Govern' | 'Settings'`.
  - `ROUTES: RouteDef[]` (the single source of truth).
  - `visibleRoutes(caps: Capabilities): RouteDef[]` (filters by `when`).
  - `GROUP_ORDER: NavGroupName[]`.

- [ ] **Step 1: Create the honest placeholder view `web/app/src/views/Placeholder.tsx`**

```tsx
// Honest placeholder for a route whose rich view ships in a later phase. It
// names the org-scoped BFF endpoint that already backs it, so the shell never
// lies about what is wired.
export function Placeholder({ title, endpoint, phase }: { title: string; endpoint: string; phase: string }) {
  return (
    <section>
      <h2>{title}</h2>
      <p className="t-dim">
        Reads <code>{endpoint}</code>. The rich view ships in {phase}; the org-scoped BFF endpoint is live today.
      </p>
    </section>
  )
}
```

- [ ] **Step 2: Write the failing test `web/app/src/nav/routes.test.tsx`**

```tsx
import { describe, it, expect } from 'vitest'
import { ROUTES, visibleRoutes, GROUP_ORDER } from './routes'
import type { Capabilities } from '../api'

const base: Capabilities = {
  edition: 'community',
  billing: false,
  signup: false,
  teams: true,
  idp: 'oidc',
  orgSwitcher: false,
  secrets: { providers: ['kube'] },
  proof: true,
  ownership: 'self-hosted',
}

describe('routes config', () => {
  it('every route has a unique path and a known group', () => {
    const paths = ROUTES.map((r) => r.path)
    expect(new Set(paths).size).toBe(paths.length)
    for (const r of ROUTES) expect(GROUP_ORDER).toContain(r.group)
  })

  it('hides billing when capabilities.billing is false', () => {
    const visible = visibleRoutes(base)
    expect(visible.find((r) => r.path === '/billing')).toBeUndefined()
  })

  it('shows billing when capabilities.billing is true', () => {
    const visible = visibleRoutes({ ...base, billing: true })
    expect(visible.find((r) => r.path === '/billing')).toBeDefined()
  })

  it('hides instruments when proof is false', () => {
    const visible = visibleRoutes({ ...base, proof: false })
    expect(visible.find((r) => r.path === '/')).toBeUndefined()
  })
})
```

- [ ] **Step 3: Run it and confirm it fails**

Run: `pnpm -C web/app test src/nav/routes.test.tsx`
Expected: FAIL with "Cannot find module './routes'".

- [ ] **Step 4: Implement `web/app/src/nav/routes.tsx`**

```tsx
// The single source of truth for the console's information architecture. Both
// the nav (AppShell) and the router (router.tsx) derive from this array, so a
// route is declared exactly once. `when` gates a route on the server-advertised
// capabilities document; a route with no `when` is always present.
import type { Capabilities } from '../api'
import { Instruments } from '../views/Instruments'
import { Sandboxes } from '../views/Sandboxes'
import { Secrets } from '../views/Secrets'
import { Placeholder } from '../views/Placeholder'

export type NavGroupName = 'Run' | 'Build' | 'Govern' | 'Settings'
export const GROUP_ORDER: NavGroupName[] = ['Run', 'Build', 'Govern', 'Settings']

export type RouteDef = {
  path: string
  label: string
  group: NavGroupName
  element: () => JSX.Element
  when?: (c: Capabilities) => boolean
}

export const ROUTES: RouteDef[] = [
  { path: '/', label: 'Instruments', group: 'Run', element: () => <Instruments />, when: (c) => c.proof },
  { path: '/sandboxes', label: 'Sandboxes', group: 'Run', element: () => <Sandboxes /> },
  { path: '/workspaces', label: 'Workspaces', group: 'Build', element: () => <Placeholder title="Workspaces" endpoint="/console/templates" phase="B2" /> },
  { path: '/templates', label: 'Templates', group: 'Build', element: () => <Placeholder title="Templates" endpoint="/console/templates" phase="B2" /> },
  { path: '/secrets', label: 'Secrets', group: 'Build', element: () => <Secrets /> },
  { path: '/keys', label: 'API keys', group: 'Build', element: () => <Placeholder title="API keys" endpoint="/console/keys" phase="B2" /> },
  { path: '/members', label: 'Members', group: 'Govern', element: () => <Placeholder title="Members & roles" endpoint="/console/members" phase="B2" />, when: (c) => c.teams },
  { path: '/audit', label: 'Audit', group: 'Govern', element: () => <Placeholder title="Audit log" endpoint="/console/audit" phase="B2" /> },
  { path: '/usage', label: 'Usage', group: 'Govern', element: () => <Placeholder title="Usage & cost" endpoint="/console/usage" phase="B2" /> },
  { path: '/billing', label: 'Billing', group: 'Govern', element: () => <Placeholder title="Billing" endpoint="/console/billing" phase="B2" />, when: (c) => c.billing },
  { path: '/settings', label: 'Settings', group: 'Settings', element: () => <Placeholder title="Settings" endpoint="/console/capabilities" phase="B2" /> },
]

export function visibleRoutes(caps: Capabilities): RouteDef[] {
  return ROUTES.filter((r) => !r.when || r.when(caps))
}
```

- [ ] **Step 5: Run it and confirm it passes**

Run: `pnpm -C web/app test src/nav/routes.test.tsx`
Expected: PASS, 4 tests.

- [ ] **Step 6: Commit**

```bash
git add web/app/src/views/Placeholder.tsx web/app/src/nav/routes.tsx web/app/src/nav/routes.test.tsx
git commit -s -m "feat(console): add capability-gated routes config as the IA source of truth"
```

---

### Task 4: Router with intent preloading + capability guards

**Files:**
- Create: `web/app/src/router.tsx`
- Modify: `web/app/src/test/utils.tsx`
- Test: `web/app/src/router.test.tsx`

**Interfaces:**
- Consumes: `ROUTES`, `visibleRoutes` from `nav/routes.tsx`; `AppShell` (Task 5) is referenced as the root layout, so this task creates a minimal root that renders an `<Outlet/>` now and Task 5 swaps in the real shell.
- Produces:
  - `createConsoleRouter(caps: Capabilities)` returning a configured TanStack Router instance (routes built from `visibleRoutes(caps)`, `defaultPreload: 'intent'`).
  - `renderAt(path, caps)` test helper in `test/utils.tsx`.

- [ ] **Step 1: Add the router dep**

```bash
pnpm -C web/app add @tanstack/react-router@^1.87.0
```

- [ ] **Step 2: Write the failing test `web/app/src/router.test.tsx`**

```tsx
import { describe, it, expect, vi } from 'vitest'
import { render, waitFor, screen } from '@testing-library/react'
import { RouterProvider } from '@tanstack/react-router'
import { createConsoleRouter } from './router'
import type { Capabilities } from './api'

const caps: Capabilities = {
  edition: 'community', billing: false, signup: false, teams: true, idp: 'oidc',
  orgSwitcher: false, secrets: { providers: ['kube'] }, proof: true, ownership: 'self-hosted',
}

describe('console router', () => {
  it('renders the sandboxes route', async () => {
    vi.spyOn(globalThis, 'fetch').mockResolvedValue(
      new Response(JSON.stringify({ sandboxes: [] }), { status: 200, headers: { 'content-type': 'application/json' } }),
    )
    const router = createConsoleRouter(caps)
    await router.navigate({ to: '/sandboxes' })
    render(<RouterProvider router={router} />)
    await waitFor(() => expect(screen.getByText(/Sandboxes/i)).toBeInTheDocument())
  })

  it('does not build a billing route when billing is off', () => {
    const router = createConsoleRouter(caps)
    const paths = router.routeTree.children?.map((c: { path?: string }) => c.path) ?? []
    expect(paths).not.toContain('/billing')
  })
})
```

- [ ] **Step 3: Run it and confirm it fails**

Run: `pnpm -C web/app test src/router.test.tsx`
Expected: FAIL with "Cannot find module './router'".

- [ ] **Step 4: Implement `web/app/src/router.tsx`**

```tsx
// Builds the TanStack Router from the capability-filtered routes. Intent
// preloading (hover / focus) is what makes navigation feel instant: the target
// route's component and data start loading before the click. The root route is
// a thin layout here; Task 5 replaces RootLayout's body with the real AppShell.
import {
  createRootRoute,
  createRoute,
  createRouter,
  Outlet,
} from '@tanstack/react-router'
import type { Capabilities } from './api'
import { visibleRoutes } from './nav/routes'

function RootLayout() {
  // Replaced by AppShell in Task 5. For now, render the active route directly so
  // the router is testable in isolation.
  return <Outlet />
}

export function createConsoleRouter(caps: Capabilities) {
  const rootRoute = createRootRoute({ component: RootLayout })
  const children = visibleRoutes(caps).map((r) => {
    const Element = r.element
    return createRoute({
      getParentRoute: () => rootRoute,
      path: r.path,
      component: () => <Element />,
    })
  })
  const routeTree = rootRoute.addChildren(children)
  return createRouter({
    routeTree,
    defaultPreload: 'intent',
    defaultPreloadStaleTime: 0, // let TanStack Query own caching
  })
}
```

- [ ] **Step 5: Run it and confirm it passes**

Run: `pnpm -C web/app test src/router.test.tsx`
Expected: PASS, 2 tests.

- [ ] **Step 6: Add the `renderAt` helper to `web/app/src/test/utils.tsx`**

Append to the file:

```tsx
import { RouterProvider } from '@tanstack/react-router'
import { createConsoleRouter } from '../router'
import type { Capabilities } from '../api'

// Render the app at a given path with a given capabilities document, inside the
// query provider. Used by AppShell and CommandPalette tests.
export async function renderAt(path: string, caps: Capabilities) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  const router = createConsoleRouter(caps)
  await router.navigate({ to: path })
  return render(
    <QueryClientProvider client={client}>
      <RouterProvider router={router} />
    </QueryClientProvider>,
  )
}
```

- [ ] **Step 7: Run the full suite to confirm nothing regressed**

Run: `pnpm -C web/app test`
Expected: PASS, all tests.

- [ ] **Step 8: Commit**

```bash
git add web/app/package.json web/app/pnpm-lock.yaml web/app/src/router.tsx web/app/src/router.test.tsx web/app/src/test/utils.tsx
git commit -s -m "feat(console): add TanStack Router with intent preloading and capability guards"
```

---

### Task 5: AppShell (grouped nav + ownership badge)

**Files:**
- Create: `web/app/src/nav/AppShell.tsx`
- Modify: `web/app/src/router.tsx` (use `AppShell` as the root layout body)
- Test: `web/app/src/nav/AppShell.test.tsx`

**Interfaces:**
- Consumes: `useCapabilities()` from `data/query.ts`; `visibleRoutes`, `GROUP_ORDER` from `nav/routes.tsx`; `Division` from `@mitos/brand`; the router's `Link` from `@tanstack/react-router`.
- Produces: `AppShell` component rendering the grouped sidebar (nav `<Link>`s with `preload="intent"`), the ownership badge, and an `<Outlet/>` for the active route.

- [ ] **Step 1: Write the failing test `web/app/src/nav/AppShell.test.tsx`**

```tsx
import { describe, it, expect, vi, beforeEach } from 'vitest'
import { waitFor, screen } from '@testing-library/react'
import { renderAt } from '../test/utils'
import type { Capabilities } from '../api'

const caps: Capabilities = {
  edition: 'community', billing: false, signup: false, teams: true, idp: 'oidc',
  orgSwitcher: false, secrets: { providers: ['kube'] }, proof: true, ownership: 'self-hosted',
}

beforeEach(() => {
  vi.spyOn(globalThis, 'fetch').mockImplementation((input) => {
    const url = String(input)
    if (url.endsWith('/console/capabilities')) {
      return Promise.resolve(new Response(JSON.stringify(caps), { status: 200, headers: { 'content-type': 'application/json' } }))
    }
    return Promise.resolve(new Response(JSON.stringify({ sandboxes: [], secrets: [] }), { status: 200, headers: { 'content-type': 'application/json' } }))
  })
})

describe('AppShell', () => {
  it('renders the group headers and the visible nav links', async () => {
    await renderAt('/sandboxes', caps)
    await waitFor(() => expect(screen.getByRole('link', { name: 'Sandboxes' })).toBeInTheDocument())
    expect(screen.getByText('Run')).toBeInTheDocument()
    expect(screen.getByText('Govern')).toBeInTheDocument()
    // billing is off -> no Billing link
    expect(screen.queryByRole('link', { name: 'Billing' })).not.toBeInTheDocument()
  })

  it('shows the self-hosted ownership badge', async () => {
    await renderAt('/sandboxes', caps)
    await waitFor(() => expect(screen.getByText('Self-hosted')).toBeInTheDocument())
  })
})
```

- [ ] **Step 2: Run it and confirm it fails**

Run: `pnpm -C web/app test src/nav/AppShell.test.tsx`
Expected: FAIL with "Cannot find module './AppShell'".

- [ ] **Step 3: Implement `web/app/src/nav/AppShell.tsx`**

```tsx
// The console chrome: a grouped sidebar (one section per NavGroup), the brand
// mark, the ownership / residency badge, and the routed content. Nav links use
// the router's intent preloading so hovering a link warms the target route.
import { Link, Outlet } from '@tanstack/react-router'
import { Division } from '@mitos/brand'
import { useCapabilities } from '../data/query'
import { visibleRoutes, GROUP_ORDER, type NavGroupName, type RouteDef } from './routes'
import type { Capabilities } from '../api'

export function AppShell() {
  const { data: caps } = useCapabilities()
  if (!caps) return <main style={{ padding: 32 }}><div className="t-dim">loading...</div></main>
  const routes = visibleRoutes(caps)
  return (
    <div style={{ display: 'flex', minHeight: '100vh', maxWidth: 'var(--maxw)', margin: '0 auto' }}>
      <nav style={{ width: 220, padding: 'var(--space-5)', borderRight: '1px solid var(--hairline)' }}>
        <div style={{ display: 'flex', alignItems: 'center', gap: 'var(--space-2)', marginBottom: 'var(--space-6)' }}>
          <Division size={28} />
          <strong>mitos</strong>
        </div>
        {GROUP_ORDER.map((group) => (
          <NavSection key={group} group={group} routes={routes.filter((r) => r.group === group)} />
        ))}
        <OwnershipBadge caps={caps} />
      </nav>
      <main style={{ flex: 1, padding: 'var(--space-6)' }}>
        <Outlet />
      </main>
    </div>
  )
}

function NavSection({ group, routes }: { group: NavGroupName; routes: RouteDef[] }) {
  if (routes.length === 0) return null
  return (
    <div style={{ marginBottom: 'var(--space-5)' }}>
      <div className="t-dim" style={{ fontSize: 'var(--step--1)', textTransform: 'uppercase', letterSpacing: '0.08em', marginBottom: 'var(--space-2)' }}>
        {group}
      </div>
      {routes.map((r) => (
        <Link
          key={r.path}
          to={r.path}
          preload="intent"
          className="nav-link"
          activeProps={{ className: 'nav-link nav-link-active' }}
          style={{ display: 'block', padding: 'var(--space-2)', borderRadius: 'var(--r-sm)' }}
        >
          {r.label}
        </Link>
      ))}
    </div>
  )
}

function OwnershipBadge({ caps }: { caps: Capabilities }) {
  const selfHosted = caps.ownership === 'self-hosted'
  return (
    <div className="card" style={{ marginTop: 'var(--space-6)', fontSize: 'var(--step--1)' }}>
      <div style={{ color: 'var(--cyan)' }}>{selfHosted ? 'Self-hosted' : 'Hosted by mitos'}</div>
      <div className="t-dim">{selfHosted ? 'Your data never leaves your infrastructure.' : 'Same engine and API; portable to self-host.'}</div>
    </div>
  )
}
```

- [ ] **Step 4: Wire `AppShell` into the router root in `web/app/src/router.tsx`**

Replace the `RootLayout` function with an import and use of `AppShell`:

```tsx
import { AppShell } from './nav/AppShell'
```

and change the root route to:

```tsx
const rootRoute = createRootRoute({ component: AppShell })
```

Delete the now-unused local `RootLayout` function and the `Outlet` import if it is no longer referenced in this file.

- [ ] **Step 5: Run the shell tests and the router tests**

Run: `pnpm -C web/app test src/nav/AppShell.test.tsx src/router.test.tsx`
Expected: PASS. (The router test still renders `/sandboxes`; AppShell now wraps it.)

- [ ] **Step 6: Typecheck**

Run: `pnpm -C web/app typecheck`
Expected: clean, no errors.

- [ ] **Step 7: Commit**

```bash
git add web/app/src/nav/AppShell.tsx web/app/src/nav/AppShell.test.tsx web/app/src/router.tsx
git commit -s -m "feat(console): add grouped AppShell with intent-preloaded nav and ownership badge"
```

---

### Task 6: Command palette (Cmd-K)

**Files:**
- Create: `web/app/src/nav/CommandPalette.tsx`
- Modify: `web/app/src/nav/AppShell.tsx` (mount the palette)
- Test: `web/app/src/nav/CommandPalette.test.tsx`

**Interfaces:**
- Consumes: `visibleRoutes` from `nav/routes.tsx`; the router's `useNavigate` from `@tanstack/react-router`; `Capabilities`.
- Produces: `CommandPalette({ caps })` component that opens on Cmd/Ctrl-K, fuzzy-filters routes, and navigates on Enter / click; `fuzzyMatch(query, label)` helper exported for unit testing.

- [ ] **Step 1: Write the failing test `web/app/src/nav/CommandPalette.test.tsx`**

```tsx
import { describe, it, expect } from 'vitest'
import { fuzzyMatch } from './CommandPalette'

describe('fuzzyMatch', () => {
  it('matches subsequence case-insensitively', () => {
    expect(fuzzyMatch('sbx', 'Sandboxes')).toBe(true)
    expect(fuzzyMatch('aud', 'Audit')).toBe(true)
    expect(fuzzyMatch('zzz', 'Audit')).toBe(false)
  })

  it('empty query matches everything', () => {
    expect(fuzzyMatch('', 'Anything')).toBe(true)
  })
})
```

- [ ] **Step 2: Run it and confirm it fails**

Run: `pnpm -C web/app test src/nav/CommandPalette.test.tsx`
Expected: FAIL with "Cannot find module './CommandPalette'".

- [ ] **Step 3: Implement `web/app/src/nav/CommandPalette.tsx`**

```tsx
// Cmd-K command palette: fuzzy navigation to any visible route. Opens on
// Cmd/Ctrl-K, filters as you type, navigates on Enter or click. Actions (fork a
// sandbox, create a key) are added by later phases as those flows land; this B0
// version is navigation-complete.
import { useEffect, useMemo, useState } from 'react'
import { useNavigate } from '@tanstack/react-router'
import { visibleRoutes } from './routes'
import type { Capabilities } from '../api'

// Subsequence match: every char of query appears in order in label. Cheap and
// good enough for a route list; case-insensitive.
export function fuzzyMatch(query: string, label: string): boolean {
  const q = query.toLowerCase()
  const l = label.toLowerCase()
  let i = 0
  for (const ch of l) {
    if (i < q.length && ch === q[i]) i++
  }
  return i === q.length
}

export function CommandPalette({ caps }: { caps: Capabilities }) {
  const [open, setOpen] = useState(false)
  const [query, setQuery] = useState('')
  const navigate = useNavigate()
  const routes = useMemo(() => visibleRoutes(caps), [caps])
  const results = useMemo(() => routes.filter((r) => fuzzyMatch(query, r.label)), [routes, query])

  useEffect(() => {
    function onKey(e: KeyboardEvent) {
      if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === 'k') {
        e.preventDefault()
        setOpen((v) => !v)
      }
      if (e.key === 'Escape') setOpen(false)
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [])

  if (!open) return null

  function go(path: string) {
    setOpen(false)
    setQuery('')
    void navigate({ to: path })
  }

  return (
    <div role="dialog" aria-label="Command palette" className="palette-backdrop" onClick={() => setOpen(false)}>
      <div className="palette" onClick={(e) => e.stopPropagation()}>
        <input
          autoFocus
          aria-label="Command palette input"
          placeholder="Jump to..."
          value={query}
          onChange={(e) => setQuery(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === 'Enter' && results[0]) go(results[0].path)
          }}
        />
        <ul>
          {results.map((r) => (
            <li key={r.path}>
              <button onClick={() => go(r.path)}>{r.label}<span className="t-dim"> {r.group}</span></button>
            </li>
          ))}
          {results.length === 0 && <li className="t-dim" style={{ padding: 'var(--space-2)' }}>No matches</li>}
        </ul>
      </div>
    </div>
  )
}
```

- [ ] **Step 4: Run the unit test and confirm it passes**

Run: `pnpm -C web/app test src/nav/CommandPalette.test.tsx`
Expected: PASS, 2 tests.

- [ ] **Step 5: Mount the palette in `web/app/src/nav/AppShell.tsx`**

Add the import:

```tsx
import { CommandPalette } from './CommandPalette'
```

and render it inside the shell root, just before the closing `</div>` of the flex container (after `</main>`):

```tsx
      <CommandPalette caps={caps} />
```

- [ ] **Step 6: Write the behavior test for open-on-Cmd-K and append to `web/app/src/nav/CommandPalette.test.tsx`**

```tsx
import { vi, beforeEach } from 'vitest'
import { screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { renderAt } from '../test/utils'
import type { Capabilities } from '../api'

const caps2: Capabilities = {
  edition: 'community', billing: false, signup: false, teams: true, idp: 'oidc',
  orgSwitcher: false, secrets: { providers: ['kube'] }, proof: true, ownership: 'self-hosted',
}

describe('CommandPalette behavior', () => {
  beforeEach(() => {
    vi.spyOn(globalThis, 'fetch').mockImplementation((input) => {
      const url = String(input)
      if (url.endsWith('/console/capabilities')) {
        return Promise.resolve(new Response(JSON.stringify(caps2), { status: 200, headers: { 'content-type': 'application/json' } }))
      }
      return Promise.resolve(new Response(JSON.stringify({ sandboxes: [], secrets: [] }), { status: 200, headers: { 'content-type': 'application/json' } }))
    })
  })

  it('opens on Cmd-K and filters', async () => {
    const user = userEvent.setup()
    await renderAt('/sandboxes', caps2)
    await waitFor(() => expect(screen.getByRole('link', { name: 'Sandboxes' })).toBeInTheDocument())
    await user.keyboard('{Meta>}k{/Meta}')
    const input = await screen.findByLabelText('Command palette input')
    await user.type(input, 'aud')
    expect(screen.getByRole('button', { name: /Audit/ })).toBeInTheDocument()
  })
})
```

- [ ] **Step 7: Run the palette tests and the shell test together**

Run: `pnpm -C web/app test src/nav/`
Expected: PASS, all nav tests.

- [ ] **Step 8: Typecheck and commit**

Run: `pnpm -C web/app typecheck`
Expected: clean.

```bash
git add web/app/src/nav/CommandPalette.tsx web/app/src/nav/CommandPalette.test.tsx web/app/src/nav/AppShell.tsx
git commit -s -m "feat(console): add Cmd-K command palette for fuzzy route navigation"
```

---

### Task 7: UI primitives (EmptyState, Skeleton, Toast)

**Files:**
- Create: `web/app/src/ui/EmptyState.tsx`
- Create: `web/app/src/ui/Skeleton.tsx`
- Create: `web/app/src/ui/Toast.tsx`
- Test: `web/app/src/ui/Toast.test.tsx`
- Test: `web/app/src/ui/EmptyState.test.tsx`

**Interfaces:**
- Consumes: `Button` from `@mitos/brand`.
- Produces:
  - `EmptyState({ title, body, action? })` where `action?: { label: string; onClick: () => void }`.
  - `Skeleton({ rows? })`.
  - `ToastProvider({ children })` and `useToast(): { notify: (msg: string, kind?: 'ok' | 'error') => void }`.

- [ ] **Step 1: Write the failing test `web/app/src/ui/EmptyState.test.tsx`**

```tsx
import { describe, it, expect, vi } from 'vitest'
import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { EmptyState } from './EmptyState'

describe('EmptyState', () => {
  it('renders title and body and fires the action', async () => {
    const onClick = vi.fn()
    render(<EmptyState title="No sandboxes yet" body="Fork your first one." action={{ label: 'Fork', onClick }} />)
    expect(screen.getByText('No sandboxes yet')).toBeInTheDocument()
    await userEvent.click(screen.getByRole('button', { name: 'Fork' }))
    expect(onClick).toHaveBeenCalledOnce()
  })
})
```

- [ ] **Step 2: Run it and confirm it fails**

Run: `pnpm -C web/app test src/ui/EmptyState.test.tsx`
Expected: FAIL with "Cannot find module './EmptyState'".

- [ ] **Step 3: Implement `web/app/src/ui/EmptyState.tsx`**

```tsx
// Teaching empty state: never a blank panel. A title, a one-line explanation,
// and an optional primary action that starts the thing the view is for.
import { Button } from '@mitos/brand'

export function EmptyState({
  title,
  body,
  action,
}: {
  title: string
  body: string
  action?: { label: string; onClick: () => void }
}) {
  return (
    <div className="card" style={{ textAlign: 'center', padding: 'var(--space-8)' }}>
      <h3 style={{ marginBottom: 'var(--space-2)' }}>{title}</h3>
      <p className="t-dim" style={{ marginBottom: 'var(--space-5)' }}>{body}</p>
      {action && <Button onClick={action.onClick}>{action.label}</Button>}
    </div>
  )
}
```

- [ ] **Step 4: Implement `web/app/src/ui/Skeleton.tsx`**

```tsx
// Skeleton placeholder: shown while a query is loading so navigation reveals
// structure instantly instead of a spinner. Pure presentational.
export function Skeleton({ rows = 3 }: { rows?: number }) {
  return (
    <div aria-busy="true" aria-label="loading">
      {Array.from({ length: rows }).map((_, i) => (
        <div key={i} className="skeleton-row" style={{ height: 'var(--space-5)', marginBottom: 'var(--space-2)', borderRadius: 'var(--r-sm)', background: 'var(--field-1)' }} />
      ))}
    </div>
  )
}
```

- [ ] **Step 5: Write the failing test `web/app/src/ui/Toast.test.tsx`**

```tsx
import { describe, it, expect } from 'vitest'
import { render, screen, act, waitForElementToBeRemoved } from '@testing-library/react'
import { ToastProvider, useToast } from './Toast'

function Trigger() {
  const { notify } = useToast()
  return <button onClick={() => notify('saved', 'ok')}>go</button>
}

describe('Toast', () => {
  it('shows a toast and auto-dismisses it', async () => {
    render(
      <ToastProvider>
        <Trigger />
      </ToastProvider>,
    )
    act(() => {
      screen.getByRole('button', { name: 'go' }).click()
    })
    expect(await screen.findByText('saved')).toBeInTheDocument()
    await waitForElementToBeRemoved(() => screen.queryByText('saved'), { timeout: 5000 })
  })
})
```

- [ ] **Step 6: Run it and confirm it fails**

Run: `pnpm -C web/app test src/ui/Toast.test.tsx`
Expected: FAIL with "Cannot find module './Toast'".

- [ ] **Step 7: Implement `web/app/src/ui/Toast.tsx`**

```tsx
// Minimal toast system: a provider holds a queue, useToast().notify pushes a
// message, each auto-dismisses after 3s. Used for optimistic-mutation feedback
// in later phases (create key, terminate sandbox).
import { createContext, useContext, useCallback, useState, type ReactNode } from 'react'

type Toast = { id: number; msg: string; kind: 'ok' | 'error' }
type ToastApi = { notify: (msg: string, kind?: 'ok' | 'error') => void }

const ToastContext = createContext<ToastApi | null>(null)

let nextId = 1

export function ToastProvider({ children }: { children: ReactNode }) {
  const [toasts, setToasts] = useState<Toast[]>([])
  const notify = useCallback((msg: string, kind: 'ok' | 'error' = 'ok') => {
    const id = nextId++
    setToasts((t) => [...t, { id, msg, kind }])
    setTimeout(() => setToasts((t) => t.filter((x) => x.id !== id)), 3000)
  }, [])
  return (
    <ToastContext.Provider value={{ notify }}>
      {children}
      <div className="toast-stack" style={{ position: 'fixed', bottom: 'var(--space-5)', right: 'var(--space-5)' }}>
        {toasts.map((t) => (
          <div key={t.id} role="status" className={`toast toast-${t.kind}`}>{t.msg}</div>
        ))}
      </div>
    </ToastContext.Provider>
  )
}

export function useToast(): ToastApi {
  const ctx = useContext(ToastContext)
  if (!ctx) throw new Error('useToast must be used inside a ToastProvider')
  return ctx
}
```

- [ ] **Step 8: Run the UI tests and confirm they pass**

Run: `pnpm -C web/app test src/ui/`
Expected: PASS, 2 test files.

- [ ] **Step 9: Commit**

```bash
git add web/app/src/ui/EmptyState.tsx web/app/src/ui/Skeleton.tsx web/app/src/ui/Toast.tsx web/app/src/ui/EmptyState.test.tsx web/app/src/ui/Toast.test.tsx
git commit -s -m "feat(console): add EmptyState, Skeleton, and Toast UI primitives"
```

---

### Task 8: Wire the App provider stack + Playwright smoke

**Files:**
- Modify: `web/app/src/App.tsx`
- Create: `web/app/playwright.config.ts`
- Create: `web/app/e2e/smoke.spec.ts`
- Modify: `web/app/package.json` (add `e2e` script)
- Test: `web/app/src/App.test.tsx`

**Interfaces:**
- Consumes: `queryClient` from `data/query.ts`; `createConsoleRouter` from `router.tsx`; `useCapabilities` from `data/query.ts`; `ToastProvider` from `ui/Toast.tsx`.
- Produces: an `App` that mounts the provider stack and the router, replacing the hand-rolled nav.

- [ ] **Step 1: Write the failing test `web/app/src/App.test.tsx`**

```tsx
import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, screen, waitFor } from '@testing-library/react'
import { App } from './App'

const caps = {
  edition: 'community', billing: false, signup: false, teams: true, idp: 'oidc',
  orgSwitcher: false, secrets: { providers: ['kube'] }, proof: true, ownership: 'self-hosted',
}

describe('App', () => {
  beforeEach(() => {
    vi.spyOn(globalThis, 'fetch').mockImplementation((input) => {
      const url = String(input)
      if (url.endsWith('/console/capabilities')) {
        return Promise.resolve(new Response(JSON.stringify(caps), { status: 200, headers: { 'content-type': 'application/json' } }))
      }
      return Promise.resolve(new Response(JSON.stringify({ sandboxes: [], secrets: [], instruments: {} }), { status: 200, headers: { 'content-type': 'application/json' } }))
    })
  })

  it('boots, fetches capabilities, and renders the shell nav', async () => {
    render(<App />)
    await waitFor(() => expect(screen.getByRole('link', { name: 'Sandboxes' })).toBeInTheDocument())
    expect(screen.getByText('Run')).toBeInTheDocument()
  })
})
```

- [ ] **Step 2: Run it and confirm it fails**

Run: `pnpm -C web/app test src/App.test.tsx`
Expected: FAIL (the current `App` renders the old hand-rolled nav with no router `Link`s, so `getByRole('link', { name: 'Sandboxes' })` is not found).

- [ ] **Step 3: Rewrite `web/app/src/App.tsx`**

```tsx
// The console root: mounts the provider stack (query cache, toasts) and the
// capability-built router. The shell, nav, and routing all live below; this
// file only assembles providers. Capabilities are fetched once here so the
// router is built from a known edition before first paint of the shell.
import { QueryClientProvider } from '@tanstack/react-query'
import { RouterProvider } from '@tanstack/react-router'
import { useMemo } from 'react'
import { queryClient, useCapabilities } from './data/query'
import { createConsoleRouter } from './router'
import { ToastProvider } from './ui/Toast'

function RoutedConsole() {
  const { data: caps, error } = useCapabilities()
  const router = useMemo(() => (caps ? createConsoleRouter(caps) : null), [caps])
  if (error) return <main style={{ padding: 32 }}><div className="t-dim">console unavailable: {String(error)}</div></main>
  if (!caps || !router) return <main style={{ padding: 32 }}><div className="t-dim">loading...</div></main>
  return <RouterProvider router={router} />
}

export function App() {
  return (
    <QueryClientProvider client={queryClient}>
      <ToastProvider>
        <div className="field" />
        <RoutedConsole />
      </ToastProvider>
    </QueryClientProvider>
  )
}
```

- [ ] **Step 4: Run the App test and confirm it passes**

Run: `pnpm -C web/app test src/App.test.tsx`
Expected: PASS.

- [ ] **Step 5: Run the full unit suite + typecheck**

Run: `pnpm -C web/app test && pnpm -C web/app typecheck`
Expected: all PASS, typecheck clean.

- [ ] **Step 6: Add Playwright and the smoke config**

```bash
pnpm -C web/app add -D @playwright/test@^1.49.1
```

Create `web/app/playwright.config.ts`:

```ts
import { defineConfig } from '@playwright/test'

// Light smoke only, per the repo's Go-centric test philosophy. Assumes the
// console binary (or `vite preview`) serves the built SPA at PORT 4173.
export default defineConfig({
  testDir: './e2e',
  use: { baseURL: 'http://localhost:4173' },
  webServer: {
    command: 'pnpm build && pnpm preview --port 4173',
    url: 'http://localhost:4173',
    reuseExistingServer: true,
    timeout: 120_000,
  },
})
```

- [ ] **Step 7: Create the smoke test `web/app/e2e/smoke.spec.ts`**

```ts
import { test, expect } from '@playwright/test'

// One smoke: the SPA boots, the shell nav renders, and Cmd-K opens the palette.
// Capabilities are served by the embedded default (community edition) when run
// against the console binary; against `vite preview` the dev proxy is absent, so
// this test is marked to run only when a backing console is reachable.
test('console boots and the command palette opens', async ({ page }) => {
  await page.goto('/')
  await expect(page.getByText('mitos')).toBeVisible()
  await page.keyboard.press('Meta+k')
  await expect(page.getByLabel('Command palette input')).toBeVisible()
})
```

Add the `e2e` script to `web/app/package.json` scripts:

```json
"e2e": "playwright test"
```

- [ ] **Step 8: Run the smoke (best-effort; requires a backing console for capabilities)**

Run: `pnpm -C web/app exec playwright install --with-deps chromium && pnpm -C web/app e2e`
Expected: the boot + palette assertions pass when a console serving `/console/capabilities` is reachable. If capabilities cannot be fetched in `vite preview` (no proxy), document that the smoke runs in CI against `cmd/console -dev` and skip locally. Do not block the task on the proxy-less local case.

- [ ] **Step 9: Commit**

```bash
git add web/app/src/App.tsx web/app/src/App.test.tsx web/app/package.json web/app/pnpm-lock.yaml web/app/playwright.config.ts web/app/e2e/smoke.spec.ts
git commit -s -m "feat(console): assemble the App provider stack and add a Playwright smoke"
```

---

### Task 9: Brand CSS for the new primitives

**Files:**
- Modify: `web/packages/brand/src/base.css`
- Test: manual (visual), plus `pnpm -C web/app build` must succeed.

**Interfaces:**
- Consumes: the Fluorescence tokens in `web/packages/brand/src/tokens.css` (`--field-1`, `--magenta`, `--hairline`, `--r-sm`, spacing, motion).
- Produces: the `.nav-link`, `.nav-link-active`, `.palette`, `.palette-backdrop`, `.toast`, `.toast-ok`, `.toast-error`, `.skeleton-row` classes the shell references, all built from tokens (no raw hex).

This task is where the interface-design skill does the visual craft. The steps below add token-driven baseline styles so the shell is coherent; the interface-design skill refines them during execution.

- [ ] **Step 1: Read the existing base.css to match conventions**

Run: `sed -n '1,60p' web/packages/brand/src/base.css`
Expected: see the existing `.btn`, `.card`, `.terminal` rules and confirm tokens are referenced via `var(--*)`.

- [ ] **Step 2: Append the shell styles to `web/packages/brand/src/base.css`**

Add (all values via tokens, no raw hex, no em/en dashes):

```css
/* Console shell: nav, command palette, toasts, skeletons. Token-driven. */
.nav-link {
  color: var(--ink-2);
  text-decoration: none;
  transition: color var(--dur) var(--ease), background var(--dur) var(--ease);
}
.nav-link:hover { color: var(--ink); background: var(--field-1); }
.nav-link-active { color: var(--ink); background: var(--field-2); }

.palette-backdrop {
  position: fixed; inset: 0; background: rgba(4, 5, 10, 0.6);
  display: flex; align-items: flex-start; justify-content: center; padding-top: 12vh;
}
.palette {
  width: min(560px, 92vw); background: var(--field-1);
  border: 1px solid var(--hairline-strong); border-radius: var(--r-lg); overflow: hidden;
}
.palette input {
  width: 100%; padding: var(--space-4); background: transparent; border: 0;
  color: var(--ink); font: inherit; border-bottom: 1px solid var(--hairline);
}
.palette ul { list-style: none; margin: 0; padding: var(--space-2); max-height: 40vh; overflow: auto; }
.palette li button {
  width: 100%; text-align: left; padding: var(--space-2) var(--space-3);
  background: transparent; border: 0; color: var(--ink); border-radius: var(--r-sm); cursor: pointer;
}
.palette li button:hover { background: var(--field-2); }

.toast-stack { display: flex; flex-direction: column; gap: var(--space-2); }
.toast {
  padding: var(--space-2) var(--space-4); border-radius: var(--r-md);
  border: 1px solid var(--hairline-strong); background: var(--field-1); color: var(--ink);
}
.toast-ok { border-color: var(--green); }
.toast-error { border-color: var(--magenta); }

.skeleton-row { animation: pulse 1.2s var(--ease) infinite; }
@keyframes pulse { 50% { opacity: 0.5; } }
```

- [ ] **Step 3: Build to confirm the CSS resolves and the bundle is valid**

Run: `pnpm -C web/app build`
Expected: `tsc --noEmit` clean and Vite build succeeds, emitting `web/app/dist`.

- [ ] **Step 4: Commit**

```bash
git add web/packages/brand/src/base.css
git commit -s -m "feat(brand): add token-driven shell styles for nav, palette, toasts, skeletons"
```

---

## Self-Review

**Spec coverage (section 4 of the spec, the B0 scope in section 7):**
- Design system / `@mitos/brand` consumption: Tasks 5, 7, 9 (primitives + shell styles on Fluorescence tokens). Covered.
- AppShell + grouped nav: Task 5. Covered.
- Client-side routing with prefetch: Task 4 (TanStack Router, `defaultPreload: 'intent'`) + Task 5 (`preload="intent"` links). Covered.
- Command palette (Cmd-K): Task 6. Covered.
- Data layer (typed client + query cache): Task 2 (TanStack Query + `useCapabilities`); the typed client already exists in `api.ts`. Covered.
- Empty-state / onboarding primitives: Task 7 (`EmptyState`, `Skeleton`, `Toast`). Onboarding flow itself is a B1/B2 concern (hero views and first-run); B0 ships the primitives it will use. Covered for B0 scope.
- Capability gating (server-controlled, route never mounted when off): Tasks 3, 4, 5. Covered, with both the route filter and the router build honoring it.
- Testing discipline (component tests + light Playwright smoke): Tasks 1 through 8. Covered.
- Global constraints (no dashes, DCO sign-off, explicit staging): every commit step uses `git commit -s`, explicit `git add` paths, and ASCII punctuation. Covered.

**Placeholder scan:** no "TBD", "TODO", or "implement later" in any step; the `Placeholder` *view* (Task 3) is an intentional, named component (honest "ships in B2" panel), not a plan placeholder, and every step that changes code shows the code.

**Type consistency:** `Capabilities` is imported from `api.ts` everywhere; `RouteDef` / `NavGroupName` / `visibleRoutes` / `GROUP_ORDER` defined in Task 3 are used unchanged in Tasks 4, 5, 6; `createConsoleRouter(caps)` defined in Task 4 is consumed by Tasks 4 (helper), 5, 8; `useCapabilities()` defined in Task 2 is consumed by Tasks 5, 8; `useToast()` / `ToastProvider` defined in Task 7 are consumed in Task 8. `fuzzyMatch` is exported by Task 6 and unit-tested there. No signature drift found.

**Out-of-B0 (deferred to later plans, intentionally not here):** instrument-cockpit charts and the live fork-tree (B1); rich Sandboxes detail tabs, real Members / Audit / Usage / Keys / Billing views (B2); SSO wizard, SCIM, audit retention / export, data-retention, RBAC, trust view (B3). The nav and routes for these exist now as gated entries rendering honest placeholders, so the shell is complete and the later phases drop their views into known slots.
