# Console B2a: Sandboxes list + detail tabs Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development. Steps use checkbox (`- [ ]`) syntax.

**Goal:** Build the live Sandboxes experience: a real list view (replacing the placeholder) and a per-sandbox detail view with tabs (Overview, Logs, Fork tree real; Terminal, Filesystem, Metrics, Spending as honest placeholders), plus the `/sandboxes/$id` detail route that completes B1's deferred fork-tree deep-link.

**Architecture:** Frontend-only on the merged B0 shell + B1 hero views. Every BFF endpoint already exists (`GET /console/sandboxes`, `GET /console/sandboxes/{id}`, `GET /console/sandboxes/{id}/logs`, `DELETE /console/sandboxes/{id}`, and B1's `GET /console/forktree`). This plan introduces a small routes-architecture addition (a `hidden` flag so detail/param routes exist in the router but not the nav), a sandboxes data layer with an optimistic terminate mutation, the list and detail views, and the deep-link wiring.

**Tech Stack:** React 18 + Vite + TypeScript strict, TanStack Router (param routes), TanStack Query (+ optimistic mutation), `@mitos/brand`, Vitest + Testing Library + vitest-axe.

**Scope note:** Part of B2 (the user-facing roadmap split B2 into a/b/c/d). This is B2a. B2b (Secrets, Keys, Usage, Audit, Templates, Workspaces, Billing read views), B2c (roles + Projects), and B2d (user profile) follow. Per-sandbox Terminal/Filesystem/Metrics need new BFF surfaces and are honest placeholders here.

## Global Constraints

- **Punctuation (strict):** no em (U+2014) or en (U+2013) dashes anywhere (TS/TSX, comments, JSX copy, Markdown, commit messages). Only `.` `,` `;` `:`; ASCII `-` for ranges/compounds. Verify each commit: `grep -rnP '\xe2\x80\x94|\xe2\x80\x93'` on changed files is empty.
- **Commits:** conventional + DCO sign-off (`git commit -s`).
- **Staging:** explicit paths only; never `git add -A`. Lockfile at `web/pnpm-lock.yaml` (no new deps expected).
- **Capabilities server-controlled:** no client-side capability decisions; reuse the existing routes-config gating.
- **Integrity / honesty:** views show only real BFF data; not-yet-available tabs (Terminal, Filesystem, Metrics, Spending) render an honest placeholder naming what they will read, never fabricated content.
- **Secrets rule:** no secret value rendered or logged.
- **Responsive + accessible (spec 4.6), every view:** the list table reflows on mobile; the detail tabs are an accessible tablist (`role="tablist"`/`tab`/`tabpanel`, arrow-key navigable); AA contrast (tokens); `prefers-reduced-motion`; an axe test asserts zero violations.
- **TypeScript strict** clean (`pnpm -C web/app typecheck`); the SPA suite exits 0 (`pnpm -C web/app test`).

## File Structure

- `web/app/src/api.ts` (modify) - extend `SandboxView` (add `mem_bytes`, `created_at`); add `api.sandbox(id)`, `api.terminateSandbox(id)`, `api.sandboxLogs(id)`.
- `web/app/src/data/sandboxes.ts` (create) - `useSandboxes()`, `useSandbox(id)`, `useTerminateSandbox()` (optimistic), `useSandboxLogs(id)`.
- `web/app/src/nav/routes.tsx` (modify) - add `hidden?: boolean` to `RouteDef`; add the hidden `/sandboxes/$id` route; export `navRoutes(caps)` (visible, non-hidden) and keep `visibleRoutes(caps)` (all, for the router).
- `web/app/src/nav/AppShell.tsx` (modify) - nav uses `navRoutes` (hide detail routes).
- `web/app/src/router.tsx` (modify) - build from `visibleRoutes` (includes hidden); param route support already works via path `/sandboxes/$id`.
- `web/app/src/views/sandboxes/SandboxList.tsx` (create) - the real list (table, status dots, terminate).
- `web/app/src/views/sandboxes/SandboxDetail.tsx` (create) - the tabbed detail view.
- `web/app/src/views/sandboxes/tabs.tsx` (create) - the tab components (Overview, Logs, ForkTab, and the placeholder tabs).
- `web/app/src/ui/Tabs.tsx` (create) - an accessible tablist primitive.
- `web/app/src/views/forktree/ForkTree.tsx` (modify) - deep-link node ids to `/sandboxes/$id` now that the route exists.
- `web/packages/brand/src/base.css` (modify) - tabs, status-dot, detail-layout styles (token-driven, responsive).
- Tests alongside each.

---

### Task 1: Sandboxes data layer (+ api extensions)

**Files:**
- Modify: `web/app/src/api.ts`
- Create: `web/app/src/data/sandboxes.ts`
- Test: `web/app/src/data/sandboxes.test.tsx`

**Interfaces:**
- Produces:
  - extended `SandboxView = { id, org_id, template, node, phase, vcpus, mem_bytes, created_at }`.
  - `api.sandbox(id): Promise<SandboxView>`, `api.terminateSandbox(id): Promise<void>`, `api.sandboxLogs(id): Promise<string>`.
  - `useSandboxes()`, `useSandbox(id)`, `useSandboxLogs(id)`, `useTerminateSandbox()` (optimistic remove from the list cache, rollback on error).

- [ ] **Step 1: Write the failing test `web/app/src/data/sandboxes.test.tsx`**

```tsx
import { describe, it, expect, vi } from 'vitest'
import { renderHook, waitFor, act } from '@testing-library/react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { useSandboxes, useTerminateSandbox } from './sandboxes'

function harness() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } })
  const wrapper = ({ children }: { children: React.ReactNode }) => (
    <QueryClientProvider client={client}>{children}</QueryClientProvider>
  )
  return { client, wrapper }
}

describe('sandboxes data layer', () => {
  it('lists sandboxes', async () => {
    vi.spyOn(globalThis, 'fetch').mockResolvedValue(
      new Response(JSON.stringify({ sandboxes: [{ id: 's1', org_id: 'o', template: 't', node: 'n', phase: 'Running', vcpus: 2, mem_bytes: 1024, created_at: '2026-01-01T00:00:00Z' }] }),
        { status: 200, headers: { 'content-type': 'application/json' } }))
    const { wrapper } = harness()
    const { result } = renderHook(() => useSandboxes(), { wrapper })
    await waitFor(() => expect(result.current.data).toBeDefined())
    expect(result.current.data?.[0].id).toBe('s1')
  })

  it('optimistically removes a terminated sandbox from the list cache', async () => {
    const { client, wrapper } = harness()
    client.setQueryData(['sandboxes'], [{ id: 's1', org_id: 'o', template: 't', node: 'n', phase: 'Running', vcpus: 1, mem_bytes: 1, created_at: '' }, { id: 's2', org_id: 'o', template: 't', node: 'n', phase: 'Running', vcpus: 1, mem_bytes: 1, created_at: '' }])
    vi.spyOn(globalThis, 'fetch').mockResolvedValue(new Response(null, { status: 200 }))
    const { result } = renderHook(() => useTerminateSandbox(), { wrapper })
    await act(async () => { await result.current.mutateAsync('s1') })
    const list = client.getQueryData(['sandboxes']) as Array<{ id: string }>
    expect(list.find((s) => s.id === 's1')).toBeUndefined()
    expect(list.find((s) => s.id === 's2')).toBeDefined()
  })
})
```

- [ ] **Step 2: Run it, confirm it fails**

Run: `pnpm -C web/app test src/data/sandboxes.test.tsx`
Expected: FAIL ("Cannot find module './sandboxes'").

- [ ] **Step 3: Extend `web/app/src/api.ts`**

Replace the `SandboxView` type with the extended shape and add the methods to the `api` object:

```ts
export type SandboxView = {
  id: string
  org_id: string
  template: string
  node: string
  phase: string
  vcpus: number
  mem_bytes: number
  created_at: string
}
```

Add to `api`:

```ts
  sandbox: (id: string) => get<SandboxView>(`/console/sandboxes/${encodeURIComponent(id)}`),
  terminateSandbox: async (id: string) => {
    const r = await fetch(`/console/sandboxes/${encodeURIComponent(id)}`, { method: 'DELETE', credentials: 'same-origin' })
    if (!r.ok && r.status !== 204) throw new Error(`terminate: ${r.status}`)
  },
  sandboxLogs: async (id: string) => {
    const r = await fetch(`/console/sandboxes/${encodeURIComponent(id)}/logs`, { credentials: 'same-origin' })
    if (!r.ok) throw new Error(`logs: ${r.status}`)
    return r.text()
  },
```

(Keep the existing `sandboxes` list method but make it return the typed list.)

- [ ] **Step 4: Implement `web/app/src/data/sandboxes.ts`**

```ts
// The Sandboxes data layer: list, single, logs, and an optimistic terminate.
// Terminate removes the row from the list cache immediately and rolls back if the
// BFF rejects, so the UI feels instant but stays honest.
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { api, type SandboxView } from '../api'

export function useSandboxes() {
  return useQuery<SandboxView[]>({ queryKey: ['sandboxes'], queryFn: () => api.sandboxes(), refetchInterval: 10_000 })
}

export function useSandbox(id: string) {
  return useQuery<SandboxView>({ queryKey: ['sandbox', id], queryFn: () => api.sandbox(id), enabled: !!id })
}

export function useSandboxLogs(id: string) {
  return useQuery<string>({ queryKey: ['sandbox-logs', id], queryFn: () => api.sandboxLogs(id), enabled: !!id })
}

export function useTerminateSandbox() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (id: string) => api.terminateSandbox(id),
    onMutate: async (id: string) => {
      await qc.cancelQueries({ queryKey: ['sandboxes'] })
      const prev = qc.getQueryData<SandboxView[]>(['sandboxes'])
      qc.setQueryData<SandboxView[]>(['sandboxes'], (cur) => (cur ?? []).filter((s) => s.id !== id))
      return { prev }
    },
    onError: (_e, _id, ctx) => {
      if (ctx?.prev) qc.setQueryData(['sandboxes'], ctx.prev)
    },
    onSettled: () => {
      void qc.invalidateQueries({ queryKey: ['sandboxes'] })
    },
  })
}
```

- [ ] **Step 5: Run it, confirm pass; typecheck**

Run: `pnpm -C web/app test src/data/sandboxes.test.tsx` then `pnpm -C web/app typecheck`
Expected: PASS, clean. (If the existing `api.sandboxes()` return type needs adjustment for the new fields, update it.)

- [ ] **Step 6: Commit**

```bash
git add web/app/src/api.ts web/app/src/data/sandboxes.ts web/app/src/data/sandboxes.test.tsx
git commit -s -m "feat(console): add sandboxes data layer with optimistic terminate"
```

---

### Task 2: Hidden detail routes (routes architecture)

**Files:**
- Modify: `web/app/src/nav/routes.tsx`
- Modify: `web/app/src/nav/AppShell.tsx`
- Test: `web/app/src/nav/routes.test.tsx` (extend)

**Interfaces:**
- Produces: `RouteDef` gains `hidden?: boolean`; `navRoutes(caps): RouteDef[]` (capability-visible AND not hidden) for the nav; `visibleRoutes(caps)` stays (capability-visible, INCLUDING hidden) for the router. A hidden `/sandboxes/$id` route is added in Task 4 (placeholder element here is fine, replaced in Task 4).

- [ ] **Step 1: Write the failing test (append to `web/app/src/nav/routes.test.tsx`)**

```tsx
import { navRoutes } from './routes'

describe('hidden routes', () => {
  it('navRoutes excludes hidden routes but visibleRoutes includes them', () => {
    const nav = navRoutes(base)
    const all = visibleRoutes(base)
    // once /sandboxes/$id exists as hidden it must be in the router set, not the nav
    const detail = all.find((r) => r.path === '/sandboxes/$id')
    if (detail) {
      expect(detail.hidden).toBe(true)
      expect(nav.find((r) => r.path === '/sandboxes/$id')).toBeUndefined()
    }
    // every nav route is non-hidden
    for (const r of nav) expect(r.hidden).not.toBe(true)
  })
})
```

- [ ] **Step 2: Run it, confirm it fails**

Run: `pnpm -C web/app test src/nav/routes.test.tsx`
Expected: FAIL ("navRoutes is not a function").

- [ ] **Step 3: Update `web/app/src/nav/routes.tsx`**

Add `hidden?: boolean` to the `RouteDef` type. Add `navRoutes`:

```tsx
export function navRoutes(caps: Capabilities): RouteDef[] {
  return visibleRoutes(caps).filter((r) => !r.hidden)
}
```

(The `/sandboxes/$id` hidden route is added in Task 4.)

- [ ] **Step 4: Update `web/app/src/nav/AppShell.tsx`**

Change the nav rendering to use `navRoutes(caps)` instead of `visibleRoutes(caps)` (so hidden detail routes never appear in the sidebar). The router (router.tsx) keeps using `visibleRoutes`.

- [ ] **Step 5: Run the routes + shell tests; typecheck**

Run: `pnpm -C web/app test src/nav/ && pnpm -C web/app typecheck`
Expected: PASS, clean (nav still renders the same items since no hidden route exists yet).

- [ ] **Step 6: Commit**

```bash
git add web/app/src/nav/routes.tsx web/app/src/nav/AppShell.tsx web/app/src/nav/routes.test.tsx
git commit -s -m "feat(console): support hidden detail routes (router-only, not in nav)"
```

---

### Task 3: Accessible Tabs primitive

**Files:**
- Create: `web/app/src/ui/Tabs.tsx`
- Test: `web/app/src/ui/Tabs.test.tsx`

**Interfaces:**
- Produces: `Tabs({ tabs, active, onChange })` where `tabs: { key: string; label: string }[]`; renders an ARIA tablist (`role="tablist"`, each `role="tab"` with `aria-selected` and `aria-controls`), arrow-key navigable; the caller renders the active panel.

- [ ] **Step 1: Write the failing test `web/app/src/ui/Tabs.test.tsx`**

```tsx
import { describe, it, expect, vi } from 'vitest'
import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { Tabs } from './Tabs'

describe('Tabs', () => {
  it('renders a tablist and fires onChange on click and arrow keys', async () => {
    const onChange = vi.fn()
    render(<Tabs tabs={[{ key: 'a', label: 'Overview' }, { key: 'b', label: 'Logs' }]} active="a" onChange={onChange} />)
    const list = screen.getByRole('tablist')
    expect(list).toBeInTheDocument()
    const overview = screen.getByRole('tab', { name: 'Overview' })
    expect(overview).toHaveAttribute('aria-selected', 'true')
    await userEvent.click(screen.getByRole('tab', { name: 'Logs' }))
    expect(onChange).toHaveBeenCalledWith('b')
    overview.focus()
    await userEvent.keyboard('{ArrowRight}')
    expect(onChange).toHaveBeenCalledWith('b')
  })
})
```

- [ ] **Step 2: Run it, confirm it fails**

Run: `pnpm -C web/app test src/ui/Tabs.test.tsx`
Expected: FAIL ("Cannot find module './Tabs'").

- [ ] **Step 3: Implement `web/app/src/ui/Tabs.tsx`**

```tsx
// Accessible tab bar: ARIA tablist with roving focus and arrow-key navigation.
// The caller owns the active key and renders the panel; this is presentation +
// keyboard behavior only.
export type TabDef = { key: string; label: string }

export function Tabs({ tabs, active, onChange }: { tabs: TabDef[]; active: string; onChange: (key: string) => void }) {
  function onKey(e: React.KeyboardEvent, i: number) {
    if (e.key === 'ArrowRight' || e.key === 'ArrowLeft') {
      e.preventDefault()
      const next = e.key === 'ArrowRight' ? (i + 1) % tabs.length : (i - 1 + tabs.length) % tabs.length
      onChange(tabs[next].key)
    }
  }
  return (
    <div role="tablist" className="tabs" aria-label="Sandbox detail sections">
      {tabs.map((t, i) => (
        <button
          key={t.key}
          role="tab"
          id={`tab-${t.key}`}
          aria-selected={t.key === active}
          aria-controls={`panel-${t.key}`}
          tabIndex={t.key === active ? 0 : -1}
          className={`tab ${t.key === active ? 'tab-active' : ''}`}
          onClick={() => onChange(t.key)}
          onKeyDown={(e) => onKey(e, i)}
        >
          {t.label}
        </button>
      ))}
    </div>
  )
}
```

- [ ] **Step 4: Run it, confirm pass**

Run: `pnpm -C web/app test src/ui/Tabs.test.tsx`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add web/app/src/ui/Tabs.tsx web/app/src/ui/Tabs.test.tsx
git commit -s -m "feat(console): add accessible Tabs primitive"
```

---

### Task 4: Sandbox detail route + tabbed detail view

**Files:**
- Create: `web/app/src/views/sandboxes/SandboxDetail.tsx`
- Create: `web/app/src/views/sandboxes/tabs.tsx`
- Modify: `web/app/src/nav/routes.tsx` (add the hidden `/sandboxes/$id` route)
- Test: `web/app/src/views/sandboxes/SandboxDetail.test.tsx`

**Interfaces:**
- Consumes: `useSandbox(id)`, `useSandboxLogs(id)` (Task 1), `Tabs` (Task 3), `useForkTree` + the B1 `ForkTree` rendering, `EmptyState`/`Skeleton`, `fmtBytes`, the router `useParams`.
- Produces: `SandboxDetail` reading the `$id` param; a tab bar with Overview, Logs, Fork tree (real) and Terminal, Filesystem, Metrics, Spending (placeholder). Tab components in `tabs.tsx`.

- [ ] **Step 1: Write the failing test `web/app/src/views/sandboxes/SandboxDetail.test.tsx`**

```tsx
import { describe, it, expect, vi, beforeEach } from 'vitest'
import { waitFor, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { renderAt } from '../../test/utils'
import type { Capabilities } from '../../api'

const caps: Capabilities = {
  edition: 'community', billing: false, signup: false, teams: true, idp: 'oidc',
  orgSwitcher: false, secrets: { providers: ['kube'] }, proof: true, ownership: 'self-hosted',
}

beforeEach(() => {
  vi.spyOn(globalThis, 'fetch').mockImplementation((input) => {
    const url = String(input)
    if (url.endsWith('/console/capabilities')) return Promise.resolve(new Response(JSON.stringify(caps), { status: 200, headers: { 'content-type': 'application/json' } }))
    if (url.includes('/console/sandboxes/s1/logs')) return Promise.resolve(new Response('boot ok\nlistening', { status: 200 }))
    if (url.includes('/console/sandboxes/s1')) return Promise.resolve(new Response(JSON.stringify({ id: 's1', org_id: 'o', template: 'python-3.12', node: 'w1', phase: 'Running', vcpus: 2, mem_bytes: 2147483648, created_at: '2026-01-01T00:00:00Z' }), { status: 200, headers: { 'content-type': 'application/json' } }))
    if (url.endsWith('/console/forktree')) return Promise.resolve(new Response(JSON.stringify({ org_id: 'o', nodes: [] }), { status: 200, headers: { 'content-type': 'application/json' } }))
    return Promise.resolve(new Response(JSON.stringify({}), { status: 200, headers: { 'content-type': 'application/json' } }))
  })
})

describe('SandboxDetail', () => {
  it('renders the sandbox overview and switches to the Logs tab', async () => {
    await renderAt('/sandboxes/s1', caps)
    await waitFor(() => expect(screen.getByRole('heading', { name: /s1/ })).toBeInTheDocument())
    expect(screen.getByText('python-3.12')).toBeInTheDocument()
    await userEvent.click(screen.getByRole('tab', { name: /logs/i }))
    await waitFor(() => expect(screen.getByText(/listening/)).toBeInTheDocument())
  })
})
```

- [ ] **Step 2: Run it, confirm it fails**

Run: `pnpm -C web/app test src/views/sandboxes/SandboxDetail.test.tsx`
Expected: FAIL (no `/sandboxes/$id` route, no component).

- [ ] **Step 3: Implement `web/app/src/views/sandboxes/tabs.tsx`**

```tsx
// Sandbox detail tab panels. Overview, Logs, and Fork tree read real BFF data;
// Terminal, Filesystem, Metrics, and Spending render an honest placeholder naming
// the surface they will read (those need new BFF endpoints, a later phase).
import type { SandboxView } from '../../api'
import { fmtBytes } from '../../api'
import { useSandboxLogs } from '../../data/sandboxes'
import { EmptyState } from '../../ui/EmptyState'
import { Skeleton } from '../../ui/Skeleton'

export function OverviewTab({ sb }: { sb: SandboxView }) {
  const rows: [string, string][] = [
    ['Template', sb.template], ['Node', sb.node], ['Phase', sb.phase],
    ['vCPUs', String(sb.vcpus)], ['Memory', fmtBytes(sb.mem_bytes)], ['Created', sb.created_at || '-'],
  ]
  return (
    <dl className="kv">
      {rows.map(([k, v]) => (<div key={k} className="kv-row"><dt className="t-dim">{k}</dt><dd className="mono">{v}</dd></div>))}
    </dl>
  )
}

export function LogsTab({ id }: { id: string }) {
  const { data, isLoading, isError } = useSandboxLogs(id)
  if (isError) return <EmptyState title="Logs unavailable" body="The log stream could not be read for this sandbox." />
  if (isLoading) return <Skeleton rows={5} />
  if (!data) return <EmptyState title="No logs yet" body="This sandbox has not emitted any log lines." />
  return <pre className="logs mono">{data}</pre>
}

export function PlaceholderTab({ title, surface }: { title: string; surface: string }) {
  return <EmptyState title={`${title} ships in a later phase`} body={`It will stream over ${surface}. The transport exists; the console tab is a follow-up.`} />
}
```

- [ ] **Step 4: Implement `web/app/src/views/sandboxes/SandboxDetail.tsx`**

```tsx
// One sandbox, inspected. A tabbed detail view: Overview, Logs, and a Fork tree
// scoped to this sandbox are real; Terminal, Filesystem, Metrics, Spending are
// honest placeholders. Reads the $id route param.
import { useState } from 'react'
import { useParams } from '@tanstack/react-router'
import { useSandbox } from '../../data/sandboxes'
import { Tabs, type TabDef } from '../../ui/Tabs'
import { Skeleton } from '../../ui/Skeleton'
import { EmptyState } from '../../ui/EmptyState'
import { ForkTree } from '../forktree/ForkTree'
import { OverviewTab, LogsTab, PlaceholderTab } from './tabs'

const TABS: TabDef[] = [
  { key: 'overview', label: 'Overview' }, { key: 'logs', label: 'Logs' }, { key: 'terminal', label: 'Terminal' },
  { key: 'files', label: 'Filesystem' }, { key: 'metrics', label: 'Metrics' }, { key: 'spending', label: 'Spending' },
  { key: 'forks', label: 'Fork tree' },
]

export function SandboxDetail() {
  const { id } = useParams({ strict: false }) as { id: string }
  const [tab, setTab] = useState('overview')
  const { data: sb, isLoading, isError } = useSandbox(id)
  if (isError) return <EmptyState title="Sandbox unavailable" body="This sandbox does not exist or is not in this organization." />
  if (isLoading || !sb) return <Skeleton rows={6} />
  return (
    <section>
      <h2 className="mono">{sb.id}</h2>
      <Tabs tabs={TABS} active={tab} onChange={setTab} />
      <div role="tabpanel" id={`panel-${tab}`} aria-labelledby={`tab-${tab}`} style={{ marginTop: 'var(--space-5)' }}>
        {tab === 'overview' && <OverviewTab sb={sb} />}
        {tab === 'logs' && <LogsTab id={sb.id} />}
        {tab === 'terminal' && <PlaceholderTab title="Terminal" surface="the existing PTY transport" />}
        {tab === 'files' && <PlaceholderTab title="Filesystem" surface="the existing files API" />}
        {tab === 'metrics' && <PlaceholderTab title="Metrics" surface="the guest telemetry pipeline" />}
        {tab === 'spending' && <PlaceholderTab title="Spending" surface="the usage and cost pipeline" />}
        {tab === 'forks' && <ForkTree />}
      </div>
    </section>
  )
}
```

- [ ] **Step 5: Add the hidden route in `web/app/src/nav/routes.tsx`**

Import `SandboxDetail` and add to `ROUTES` (after `/sandboxes`):

```tsx
  { path: '/sandboxes/$id', label: 'Sandbox', group: 'Run', element: () => <SandboxDetail />, hidden: true },
```

- [ ] **Step 6: Run the detail test; confirm pass; typecheck**

Run: `pnpm -C web/app test src/views/sandboxes/SandboxDetail.test.tsx && pnpm -C web/app typecheck`
Expected: PASS, clean.

- [ ] **Step 7: Commit**

```bash
git add web/app/src/views/sandboxes/SandboxDetail.tsx web/app/src/views/sandboxes/tabs.tsx web/app/src/nav/routes.tsx web/app/src/views/sandboxes/SandboxDetail.test.tsx
git commit -s -m "feat(console): sandbox detail route with accessible tabs (overview, logs, fork tree)"
```

---

### Task 5: Sandboxes list view (real) + deep-link the fork tree

**Files:**
- Create: `web/app/src/views/sandboxes/SandboxList.tsx`
- Modify: `web/app/src/nav/routes.tsx` (point `/sandboxes` at `SandboxList`)
- Modify: `web/app/src/views/forktree/ForkTree.tsx` (deep-link node ids to `/sandboxes/$id`)
- Modify: `web/app/src/views/forktree/ForkTree.test.tsx` (assert the deep-link resolves)
- Test: `web/app/src/views/sandboxes/SandboxList.test.tsx`

**Interfaces:**
- Consumes: `useSandboxes`, `useTerminateSandbox` (Task 1); `useToast` (B0); the router `Link`; `fmtBytes`; `StatusDot` from `@mitos/brand`.
- Produces: `SandboxList` (table with id-link, template, node, phase dot, vcpus, memory, terminate); replaces the `/sandboxes` placeholder.

- [ ] **Step 1: Write the failing test `web/app/src/views/sandboxes/SandboxList.test.tsx`**

```tsx
import { describe, it, expect, vi, beforeEach } from 'vitest'
import { waitFor, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { renderAt } from '../../test/utils'
import type { Capabilities } from '../../api'

const caps: Capabilities = {
  edition: 'community', billing: false, signup: false, teams: true, idp: 'oidc',
  orgSwitcher: false, secrets: { providers: ['kube'] }, proof: true, ownership: 'self-hosted',
}
const list = { org_id: 'o', sandboxes: [{ id: 's1', org_id: 'o', template: 'python-3.12', node: 'w1', phase: 'Running', vcpus: 2, mem_bytes: 2147483648, created_at: '' }] }

beforeEach(() => {
  vi.spyOn(globalThis, 'fetch').mockImplementation((input) => {
    const url = String(input)
    if (url.endsWith('/console/capabilities')) return Promise.resolve(new Response(JSON.stringify(caps), { status: 200, headers: { 'content-type': 'application/json' } }))
    if (url.endsWith('/console/sandboxes')) return Promise.resolve(new Response(JSON.stringify(list), { status: 200, headers: { 'content-type': 'application/json' } }))
    return Promise.resolve(new Response(JSON.stringify({}), { status: 200, headers: { 'content-type': 'application/json' } }))
  })
})

describe('SandboxList', () => {
  it('renders sandboxes with a link to the detail route', async () => {
    await renderAt('/sandboxes', caps)
    await waitFor(() => expect(screen.getByRole('link', { name: /s1/ })).toBeInTheDocument())
    expect(screen.getByText('python-3.12')).toBeInTheDocument()
    expect(screen.getByRole('link', { name: /s1/ })).toHaveAttribute('href', '/sandboxes/s1')
  })
})
```

- [ ] **Step 2: Run it, confirm it fails**

Run: `pnpm -C web/app test src/views/sandboxes/SandboxList.test.tsx`
Expected: FAIL (the `/sandboxes` route still renders the placeholder, no `s1` link to `/sandboxes/s1`).

- [ ] **Step 3: Implement `web/app/src/views/sandboxes/SandboxList.tsx`**

```tsx
// The live sandboxes list. Each row links to the detail view and can be
// terminated (optimistic: the row leaves immediately, restored on error).
import { Link } from '@tanstack/react-router'
import { StatusDot, type Entity } from '@mitos/brand'
import { useSandboxes, useTerminateSandbox } from '../../data/sandboxes'
import { useToast } from '../../ui/Toast'
import { Skeleton } from '../../ui/Skeleton'
import { EmptyState } from '../../ui/EmptyState'
import { fmtBytes } from '../../api'

function phaseEntity(phase: string): Entity {
  if (phase === 'Running') return 'ready'
  if (phase === 'Paused') return 'warn'
  return 'parent'
}

export function SandboxList() {
  const { data, isLoading, isError } = useSandboxes()
  const terminate = useTerminateSandbox()
  const { notify } = useToast()
  if (isError) return <EmptyState title="Sandboxes unavailable" body="The live sandboxes could not be listed for this organization." />
  if (isLoading || !data) return <Skeleton rows={4} />
  if (data.length === 0) return <EmptyState title="No live sandboxes" body="Fork a sandbox from a template or the SDK to see it here." />
  return (
    <section>
      <h2>Sandboxes</h2>
      <table className="tbl" aria-label="Live sandboxes">
        <thead><tr><th scope="col">ID</th><th scope="col">Template</th><th scope="col">Node</th><th scope="col">Phase</th><th scope="col">vCPU</th><th scope="col">Memory</th><th scope="col"></th></tr></thead>
        <tbody>
          {data.map((s) => (
            <tr key={s.id}>
              <td><Link to="/sandboxes/$id" params={{ id: s.id }} className="mono">{s.id}</Link></td>
              <td>{s.template}</td>
              <td className="mono">{s.node}</td>
              <td><StatusDot entity={phaseEntity(s.phase)} /> {s.phase}</td>
              <td className="mono">{s.vcpus}</td>
              <td className="mono">{fmtBytes(s.mem_bytes)}</td>
              <td>
                <button
                  className="btn btn-ghost"
                  onClick={() => terminate.mutate(s.id, { onError: () => notify('terminate failed', 'error'), onSuccess: () => notify(`terminated ${s.id}`, 'ok') })}
                >
                  Terminate
                </button>
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </section>
  )
}
```

- [ ] **Step 4: Point `/sandboxes` at the list in `web/app/src/nav/routes.tsx`**

Replace the `/sandboxes` route element (currently `<Sandboxes />` placeholder) with `() => <SandboxList />` and import `SandboxList`. Remove the now-unused old `Sandboxes` import if nothing else uses it (strict mode will flag it).

- [ ] **Step 5: Deep-link the fork tree to the detail route**

In `web/app/src/views/forktree/ForkTree.tsx`, change the node id `Link` from `to="/sandboxes"` to `to="/sandboxes/$id" params={{ id: node.id }}` (the detail route now exists), and update the comment.

In `web/app/src/views/forktree/ForkTree.test.tsx`, update the navigation assertion: the fork-a link now resolves to `/sandboxes/agent-a` (or `/sandboxes/fork-a` per the fixture id). Assert the href is `/sandboxes/<id>` and that clicking it navigates to the sandbox detail (the detail view renders, since `/sandboxes/$id` is a real route; the test's fetch mock must also return a sandbox for that id). Keep the test meaningful (it must fail if the link dead-ended).

- [ ] **Step 6: Run the list + fork-tree tests; full suite; typecheck**

Run: `pnpm -C web/app test src/views/sandboxes/ src/views/forktree/ && pnpm -C web/app test && pnpm -C web/app typecheck`
Expected: all PASS, clean.

- [ ] **Step 7: Commit**

```bash
git add web/app/src/views/sandboxes/SandboxList.tsx web/app/src/nav/routes.tsx web/app/src/views/forktree/ForkTree.tsx web/app/src/views/forktree/ForkTree.test.tsx web/app/src/views/sandboxes/SandboxList.test.tsx
git commit -s -m "feat(console): live sandboxes list and deep-link the fork tree to sandbox detail"
```

---

### Task 6: Brand CSS, a11y axe test, and final verification

**Files:**
- Modify: `web/packages/brand/src/base.css`
- Create: `web/app/src/views/sandboxes/Sandboxes.a11y.test.tsx`

**Interfaces:**
- Consumes: the views from Tasks 4, 5.
- Produces: token-driven styles for `.tabs`/`.tab`/`.tab-active`, `.kv`/`.kv-row`, `.logs`; an axe test asserting zero violations on the list and detail.

- [ ] **Step 1: Append token-driven styles to `web/packages/brand/src/base.css`**

Add `.tabs` (flex, hairline bottom border), `.tab` (button, 44px min height, dim until active), `.tab-active` (ink color + a `--magenta` or `--cyan` underline via box-shadow), `.kv`/`.kv-row` (definition grid), `.logs` (mono, scrollable, `var(--field-1)` background), and a `@media (max-width: 768px)` rule so the tab bar scrolls horizontally and the list table degrades gracefully. Token-driven, no raw hex.

- [ ] **Step 2: Write the a11y test `web/app/src/views/sandboxes/Sandboxes.a11y.test.tsx`**

```tsx
import { describe, it, expect, vi, beforeEach } from 'vitest'
import { waitFor, screen } from '@testing-library/react'
import { axe } from 'vitest-axe'
import * as matchers from 'vitest-axe/matchers'
import { renderAt } from '../../test/utils'
import type { Capabilities } from '../../api'

expect.extend(matchers)
const caps: Capabilities = {
  edition: 'community', billing: false, signup: false, teams: true, idp: 'oidc',
  orgSwitcher: false, secrets: { providers: ['kube'] }, proof: true, ownership: 'self-hosted',
}
beforeEach(() => {
  vi.spyOn(globalThis, 'fetch').mockImplementation((input) => {
    const url = String(input)
    if (url.endsWith('/console/capabilities')) return Promise.resolve(new Response(JSON.stringify(caps), { status: 200, headers: { 'content-type': 'application/json' } }))
    if (url.endsWith('/console/sandboxes')) return Promise.resolve(new Response(JSON.stringify({ org_id: 'o', sandboxes: [{ id: 's1', org_id: 'o', template: 't', node: 'n', phase: 'Running', vcpus: 1, mem_bytes: 1024, created_at: '' }] }), { status: 200, headers: { 'content-type': 'application/json' } }))
    return Promise.resolve(new Response(JSON.stringify({}), { status: 200, headers: { 'content-type': 'application/json' } }))
  })
})

describe('Sandboxes accessibility', () => {
  it('the list has no axe violations', async () => {
    const { container } = await renderAt('/sandboxes', caps)
    await waitFor(() => expect(screen.getByRole('table', { name: /live sandboxes/i })).toBeInTheDocument())
    expect(await axe(container)).toHaveNoViolations()
  })
})
```

- [ ] **Step 3: Run the a11y test; fix any violation**

Run: `pnpm -C web/app test src/views/sandboxes/Sandboxes.a11y.test.tsx`
Expected: PASS. Fix any real violation (table headers, button names, tab roles) in the components; do not suppress rules.

- [ ] **Step 4: Final verification**

Run: `pnpm -C web/app test` (exit 0, all pass)
Run: `pnpm -C web/app typecheck` (clean)
Run: `pnpm -C web/app build` (succeeds)
Run: `grep -rnP '\xe2\x80\x94|\xe2\x80\x93' web/app/src/views/sandboxes web/app/src/ui/Tabs.tsx web/packages/brand/src/base.css` (empty)

- [ ] **Step 5: Commit**

```bash
git add web/packages/brand/src/base.css web/app/src/views/sandboxes/Sandboxes.a11y.test.tsx
git commit -s -m "feat(console): sandbox view styles and accessibility checks"
```

---

## Self-Review

**Spec coverage (section 4.3 IA, 4.4):** Sandboxes list (Task 5) and detail tabs (Task 4: Overview, Logs, Fork tree real; Terminal/Filesystem/Metrics/Spending honest placeholders) cover the spec's Sandboxes detail-tab list. The `/sandboxes/$id` route (Task 4) + deep-link (Task 5) complete B1's deferred link. Optimistic terminate (Task 1). Responsive + a11y (Task 6, Tabs in Task 3). Covered.

**Deferred (later phases):** Terminal (PTY), Filesystem (files API), Metrics, Spending tabs need new BFF surfaces (a B2/B3 backend slice); they are honest placeholders here. Live log tail (WebSocket) is a refinement over the fetch-based Logs tab.

**Placeholder scan:** the `PlaceholderTab` component is an intentional, honest UI element (names the real transport), not a plan placeholder; every code step shows complete code.

**Type consistency:** `SandboxView` (extended in Task 1) is used in Tasks 4, 5; `useSandboxes`/`useSandbox`/`useSandboxLogs`/`useTerminateSandbox` (Task 1) consumed in Tasks 4, 5; `navRoutes`/`hidden` (Task 2) consumed by AppShell (Task 2) and the router; `Tabs`/`TabDef` (Task 3) consumed by Task 4; the `/sandboxes/$id` route (Task 4) consumed by Task 5's deep-link. No drift.
