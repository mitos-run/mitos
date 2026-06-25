# Console B2b: read/manage views Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development. Steps use checkbox (`- [ ]`) syntax. Do NOT invoke finishing-a-development-branch; just implement, test, commit, and report.

**Goal:** Build the remaining over-existing-BFF views: API keys (create-once + revoke), Secrets (create write-only + delete), Usage & cost, Audit, Templates, and Billing (hosted). Replace their placeholders with real, polished, accessible views.

**Architecture:** Frontend-only on the merged B0/B1/B2a console. Every endpoint exists: `GET/POST /console/keys`, `POST /console/keys/{id}/revoke`, `GET/POST /console/secrets`, `DELETE /console/secrets/{name}`, `GET /console/usage`, `GET /console/audit`, `GET /console/templates`, `GET /console/billing`, `GET /console/billing/portal`. Members (roles) is B2c; Workspaces has no BFF endpoint and stays an honest placeholder.

**Tech Stack:** React 18 + Vite + TS strict, TanStack Router + Query (+ mutations), `@mitos/brand`, Vitest + Testing Library + vitest-axe.

**Scope note:** B2b of the B2 split. B2c (roles + Projects + Members), B2d (profile), B3 (enterprise) follow.

## Global Constraints

- **Punctuation (strict):** no em (U+2014) or en (U+2013) dashes anywhere. Only `.` `,` `;` `:`; ASCII `-` for compounds. Verify each commit: `grep -rnP '\xe2\x80\x94|\xe2\x80\x93'` on changed files empty.
- **Commits:** conventional + DCO sign-off (`git commit -s`).
- **Staging:** explicit paths only; never `git add -A`.
- **Secrets rule (load-bearing):** a raw API key value is shown EXACTLY ONCE on create (a `CopyOnce` affordance) and never again; the list shows only the masked prefix. A secret VALUE is never rendered or returned; the secret view shows only name/provider/version/fingerprint (write-only after create). No secret value in any log.
- **Integrity:** views render only real BFF data; no fabricated numbers.
- **Capability gating:** Billing mounts only when `caps.billing` (already gated in the routes config); keep it.
- **Responsive + accessible (spec 4.6), every view:** tables reflow on mobile; forms are labelled; AA contrast (tokens); axe asserts zero violations.
- **TypeScript strict** clean; SPA suite exits 0.

## File Structure

- `web/app/src/api.ts` (modify) - add typed shapes (`KeyView`, `UsageResponse`, `AuditEvent`, `TemplateView`, `BillingView`) + methods (`keys`, `createKey`, `revokeKey`, `usage`, `audit`, `templates`, `billing`, `billingPortal`).
- `web/app/src/data/account.ts` (create) - hooks: `useKeys`, `useCreateKey`, `useRevokeKey`, `useUsage`, `useAudit`, `useTemplates`, `useBilling`.
- `web/app/src/ui/CopyOnce.tsx` (create) - shows a one-time secret value with a copy button and a dismissable banner.
- `web/app/src/views/Keys.tsx`, `Usage.tsx`, `Audit.tsx`, `Templates.tsx`, `Billing.tsx` (create); `web/app/src/views/Secrets.tsx` (modify, make real).
- `web/app/src/nav/routes.tsx` (modify) - point `/keys`, `/usage`, `/audit`, `/templates`, `/billing`, `/secrets` at the real views.
- `web/packages/brand/src/base.css` (modify) - form, badge, and ledger styles.
- Tests alongside.

---

### Task 1: Data layer + CopyOnce primitive

**Files:**
- Modify: `web/app/src/api.ts`
- Create: `web/app/src/data/account.ts`
- Create: `web/app/src/ui/CopyOnce.tsx`
- Test: `web/app/src/data/account.test.tsx`, `web/app/src/ui/CopyOnce.test.tsx`

**Interfaces (match the Go BFF JSON exactly):**
- `KeyView = { id, name, prefix, scopes: string[], created_at, expires_at?, revoked_at?, revoked: boolean }`.
- `CreateKeyResult = { org_id, raw_key, key: KeyView }`.
- `UsageResponse = { org_id, records: unknown[], totals: Record<string, number>, cost: Record<string, number> }` (render totals/cost; records can be summarized).
- `AuditEvent = { org_id, actor_id, action, target, detail, at }`.
- `TemplateView = { name, org_id, description, image, updated_at }`.
- `BillingView = { org_id, status, balance_cents, spend_cents, soft_cap_cents, hard_cap_cents, ledger_entries: Array<{ ts?: string; cents?: number; reason?: string }> }`.
- Hooks in `account.ts`: `useKeys()`, `useCreateKey()` (returns the raw key once; invalidates `['keys']`), `useRevokeKey()` (optimistic), `useUsage()`, `useAudit()`, `useTemplates()`, `useBilling()`.
- `CopyOnce({ value, label })` - renders the value in a mono box with a Copy button and an explicit "shown once" warning.

- [ ] **Step 1: Write the failing tests**

`web/app/src/data/account.test.tsx`:

```tsx
import { describe, it, expect, vi } from 'vitest'
import { renderHook, waitFor } from '@testing-library/react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { useKeys } from './account'

function wrapper() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  return ({ children }: { children: React.ReactNode }) => <QueryClientProvider client={client}>{children}</QueryClientProvider>
}

describe('useKeys', () => {
  it('lists api keys (masked)', async () => {
    vi.spyOn(globalThis, 'fetch').mockResolvedValue(new Response(JSON.stringify({ org_id: 'o', keys: [{ id: 'k1', name: 'ci', prefix: 'mitos_live_ab12', scopes: ['sandboxes'], created_at: '', revoked: false }] }), { status: 200, headers: { 'content-type': 'application/json' } }))
    const { result } = renderHook(() => useKeys(), { wrapper: wrapper() })
    await waitFor(() => expect(result.current.data).toBeDefined())
    expect(result.current.data?.[0].prefix).toBe('mitos_live_ab12')
  })
})
```

`web/app/src/ui/CopyOnce.test.tsx`:

```tsx
import { describe, it, expect, vi } from 'vitest'
import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { CopyOnce } from './CopyOnce'

describe('CopyOnce', () => {
  it('shows the value once and copies it', async () => {
    const writeText = vi.fn().mockResolvedValue(undefined)
    Object.assign(navigator, { clipboard: { writeText } })
    render(<CopyOnce value="mitos_live_secret123" label="API key" />)
    expect(screen.getByText('mitos_live_secret123')).toBeInTheDocument()
    expect(screen.getByText(/shown once/i)).toBeInTheDocument()
    await userEvent.click(screen.getByRole('button', { name: /copy/i }))
    expect(writeText).toHaveBeenCalledWith('mitos_live_secret123')
  })
})
```

- [ ] **Step 2: Run both, confirm they fail**

Run: `pnpm -C web/app test src/data/account.test.tsx src/ui/CopyOnce.test.tsx`
Expected: FAIL (modules not found).

- [ ] **Step 3: Extend `web/app/src/api.ts`**

Add the types above and the methods (use the existing `get<T>` helper for reads; raw fetch for the mutations). For example:

```ts
export type KeyView = { id: string; name: string; prefix: string; scopes: string[]; created_at: string; expires_at?: string; revoked_at?: string; revoked: boolean }
export type CreateKeyResult = { org_id: string; raw_key: string; key: KeyView }
export type AuditEvent = { org_id: string; actor_id: string; action: string; target: string; detail: string; at: string }
export type TemplateView = { name: string; org_id: string; description: string; image: string; updated_at: string }
export type UsageResponse = { org_id: string; records: unknown[]; totals: Record<string, number>; cost: Record<string, number> }
export type BillingView = { org_id: string; status: string; balance_cents: number; spend_cents: number; soft_cap_cents: number; hard_cap_cents: number; ledger_entries: Array<{ ts?: string; cents?: number; reason?: string }> }
```

Add to `api`:

```ts
  keys: () => get<{ keys: KeyView[] }>('/console/keys').then((r) => r.keys ?? []),
  createKey: async (name: string, scopes: string[], ttlSeconds: number) => {
    const r = await fetch('/console/keys', { method: 'POST', credentials: 'same-origin', headers: { 'content-type': 'application/json' }, body: JSON.stringify({ name, scopes, ttl_seconds: ttlSeconds }) })
    if (!r.ok) throw new Error(`create key: ${r.status}`)
    return (await r.json()) as CreateKeyResult
  },
  revokeKey: async (id: string) => {
    const r = await fetch(`/console/keys/${encodeURIComponent(id)}/revoke`, { method: 'POST', credentials: 'same-origin' })
    if (!r.ok) throw new Error(`revoke: ${r.status}`)
  },
  usage: () => get<UsageResponse>('/console/usage?from=&to='),
  audit: () => get<{ events: AuditEvent[] }>('/console/audit').then((r) => r.events ?? []),
  templates: () => get<{ templates: TemplateView[] }>('/console/templates').then((r) => r.templates ?? []),
  billing: () => get<BillingView>('/console/billing'),
  billingPortal: () => get<{ url: string }>('/console/billing/portal').then((r) => r.url),
```

(Adjust the usage query string if the endpoint requires RFC3339 `from`/`to`; an empty range returns all per the BFF.)

- [ ] **Step 4: Implement `web/app/src/data/account.ts`**

```ts
// Account-scoped data: api keys (with the create-once flow), usage, audit,
// templates, and billing. Mutations invalidate their list; revoke is optimistic.
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { api, type KeyView, type AuditEvent, type TemplateView, type UsageResponse, type BillingView } from '../api'

export function useKeys() { return useQuery<KeyView[]>({ queryKey: ['keys'], queryFn: () => api.keys() }) }
export function useUsage() { return useQuery<UsageResponse>({ queryKey: ['usage'], queryFn: () => api.usage() }) }
export function useAudit() { return useQuery<AuditEvent[]>({ queryKey: ['audit'], queryFn: () => api.audit() }) }
export function useTemplates() { return useQuery<TemplateView[]>({ queryKey: ['templates'], queryFn: () => api.templates() }) }
export function useBilling() { return useQuery<BillingView>({ queryKey: ['billing'], queryFn: () => api.billing() }) }

export function useCreateKey() {
  const qc = useQueryClient()
  return useMutation({ mutationFn: (v: { name: string; scopes: string[]; ttlSeconds: number }) => api.createKey(v.name, v.scopes, v.ttlSeconds), onSuccess: () => void qc.invalidateQueries({ queryKey: ['keys'] }) })
}

export function useRevokeKey() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (id: string) => api.revokeKey(id),
    onMutate: async (id: string) => {
      await qc.cancelQueries({ queryKey: ['keys'] })
      const prev = qc.getQueryData<KeyView[]>(['keys'])
      qc.setQueryData<KeyView[]>(['keys'], (cur) => (cur ?? []).map((k) => (k.id === id ? { ...k, revoked: true } : k)))
      return { prev }
    },
    onError: (_e, _id, ctx) => { if (ctx?.prev) qc.setQueryData(['keys'], ctx.prev) },
    onSettled: () => void qc.invalidateQueries({ queryKey: ['keys'] }),
  })
}
```

- [ ] **Step 5: Implement `web/app/src/ui/CopyOnce.tsx`**

```tsx
// One-time secret reveal: shows a value with a copy button and an explicit
// "shown once" warning. Used for the raw API key on create. The caller is
// responsible for never persisting the value.
import { useState } from 'react'

export function CopyOnce({ value, label }: { value: string; label: string }) {
  const [copied, setCopied] = useState(false)
  return (
    <div className="card" style={{ borderColor: 'var(--amber)' }}>
      <div className="t-dim" style={{ fontSize: 'var(--step--1)' }}>{label} (shown once, store it now)</div>
      <div style={{ display: 'flex', gap: 'var(--space-2)', alignItems: 'center', marginTop: 'var(--space-2)' }}>
        <code className="mono" style={{ flex: 1, overflowX: 'auto' }}>{value}</code>
        <button className="btn btn-ghost" onClick={() => { void navigator.clipboard.writeText(value); setCopied(true) }}>{copied ? 'Copied' : 'Copy'}</button>
      </div>
    </div>
  )
}
```

- [ ] **Step 6: Run the tests, confirm pass; typecheck**

Run: `pnpm -C web/app test src/data/account.test.tsx src/ui/CopyOnce.test.tsx && pnpm -C web/app typecheck`
Expected: PASS, clean.

- [ ] **Step 7: Commit**

```bash
git add web/app/src/api.ts web/app/src/data/account.ts web/app/src/ui/CopyOnce.tsx web/app/src/data/account.test.tsx web/app/src/ui/CopyOnce.test.tsx
git commit -s -m "feat(console): account data layer (keys, usage, audit, templates, billing) and CopyOnce"
```

---

### Task 2: API keys view

**Files:**
- Create: `web/app/src/views/Keys.tsx`
- Modify: `web/app/src/nav/routes.tsx` (point `/keys` at `Keys`)
- Test: `web/app/src/views/Keys.test.tsx`

**Interfaces:**
- Consumes: `useKeys`, `useCreateKey`, `useRevokeKey` (Task 1), `CopyOnce`, `useToast`, `Skeleton`/`EmptyState`.
- Produces: `Keys` view: a create form (name, scopes checkboxes for `sandboxes`/`read`, TTL select), the raw key shown once via `CopyOnce` after create, a masked-prefix table with Revoke per row.

- [ ] **Step 1: Write the failing test `web/app/src/views/Keys.test.tsx`**

```tsx
import { describe, it, expect, vi, beforeEach } from 'vitest'
import { waitFor, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { renderAt } from '../test/utils'
import type { Capabilities } from '../api'

const caps: Capabilities = { edition: 'community', billing: false, signup: false, teams: true, idp: 'oidc', orgSwitcher: false, secrets: { providers: ['kube'] }, proof: true, ownership: 'self-hosted' }

beforeEach(() => {
  vi.spyOn(globalThis, 'fetch').mockImplementation((input, init) => {
    const url = String(input)
    if (url.endsWith('/console/capabilities')) return Promise.resolve(new Response(JSON.stringify(caps), { status: 200, headers: { 'content-type': 'application/json' } }))
    if (url.endsWith('/console/keys') && init?.method === 'POST') return Promise.resolve(new Response(JSON.stringify({ org_id: 'o', raw_key: 'mitos_live_RAWSECRET', key: { id: 'k2', name: 'new', prefix: 'mitos_live_new1', scopes: ['read'], created_at: '', revoked: false } }), { status: 201, headers: { 'content-type': 'application/json' } }))
    if (url.endsWith('/console/keys')) return Promise.resolve(new Response(JSON.stringify({ org_id: 'o', keys: [{ id: 'k1', name: 'ci', prefix: 'mitos_live_ab12', scopes: ['sandboxes'], created_at: '', revoked: false }] }), { status: 200, headers: { 'content-type': 'application/json' } }))
    return Promise.resolve(new Response(JSON.stringify({}), { status: 200, headers: { 'content-type': 'application/json' } }))
  })
})

describe('Keys view', () => {
  it('lists masked keys and reveals a raw key once on create', async () => {
    await renderAt('/keys', caps)
    await waitFor(() => expect(screen.getByText('mitos_live_ab12')).toBeInTheDocument())
    await userEvent.type(screen.getByLabelText(/name/i), 'new')
    await userEvent.click(screen.getByRole('button', { name: /create key/i }))
    await waitFor(() => expect(screen.getByText('mitos_live_RAWSECRET')).toBeInTheDocument())
    expect(screen.getByText(/shown once/i)).toBeInTheDocument()
  })
})
```

- [ ] **Step 2: Run it, confirm it fails**

Run: `pnpm -C web/app test src/views/Keys.test.tsx`
Expected: FAIL (placeholder renders, no create form).

- [ ] **Step 3: Implement `web/app/src/views/Keys.tsx`**

Render: a `<form>` with a name `<input aria-label="Key name">`, scope checkboxes (`sandboxes`, `read`), a TTL `<select>` (never / 30d / 90d), and a `Create key` submit; on success render `<CopyOnce value={result.raw_key} label="API key">`; a masked table (Name, Prefix, Scopes, Created, status) with a `Revoke` button per non-revoked key (optimistic via `useRevokeKey`, toast). Loading `Skeleton`, empty `EmptyState`. The raw key is held in component state only and cleared when dismissed; never refetched.

(Implementer writes the full component; the test pins the list + create-once behavior.)

- [ ] **Step 4: Point `/keys` at the view in `web/app/src/nav/routes.tsx`**

Import `Keys`, change the `/keys` route element from the placeholder to `() => <Keys />`.

- [ ] **Step 5: Run the test, full suite, typecheck**

Run: `pnpm -C web/app test src/views/Keys.test.tsx && pnpm -C web/app test && pnpm -C web/app typecheck`
Expected: all PASS, clean.

- [ ] **Step 6: Commit**

```bash
git add web/app/src/views/Keys.tsx web/app/src/nav/routes.tsx web/app/src/views/Keys.test.tsx
git commit -s -m "feat(console): API keys view with create-once reveal and revoke"
```

---

### Task 3: Secrets view (real)

**Files:**
- Modify: `web/app/src/views/Secrets.tsx`
- Modify: `web/app/src/nav/routes.tsx` (ensure `/secrets` points at the real view if not already)
- Test: `web/app/src/views/Secrets.test.tsx`

**Interfaces:**
- Consumes: the existing `api.secrets`/`api.createSecret`/`api.deleteSecret` (already in `api.ts`); `useToast`; `Skeleton`/`EmptyState`.
- Produces: `Secrets` view: a create form (name + value, write-only), a table (Name, Provider, Version, Fingerprint) with Delete per row; the value field is never echoed back after create.

- [ ] **Step 1: Write the failing test `web/app/src/views/Secrets.test.tsx`**

```tsx
import { describe, it, expect, vi, beforeEach } from 'vitest'
import { waitFor, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { renderAt } from '../test/utils'
import type { Capabilities } from '../api'

const caps: Capabilities = { edition: 'community', billing: false, signup: false, teams: true, idp: 'oidc', orgSwitcher: false, secrets: { providers: ['kube'] }, proof: true, ownership: 'self-hosted' }

beforeEach(() => {
  vi.spyOn(globalThis, 'fetch').mockImplementation((input) => {
    const url = String(input)
    if (url.endsWith('/console/capabilities')) return Promise.resolve(new Response(JSON.stringify(caps), { status: 200, headers: { 'content-type': 'application/json' } }))
    if (url.endsWith('/console/secrets')) return Promise.resolve(new Response(JSON.stringify({ org_id: 'o', secrets: [{ name: 'OPENAI_API_KEY', org_id: 'o', provider: 'kube', mode: 'copy-in', version: 2, fingerprint: 'ab12cd34' }] }), { status: 200, headers: { 'content-type': 'application/json' } }))
    return Promise.resolve(new Response(JSON.stringify({}), { status: 200, headers: { 'content-type': 'application/json' } }))
  })
})

describe('Secrets view', () => {
  it('lists secrets without revealing values', async () => {
    await renderAt('/secrets', caps)
    await waitFor(() => expect(screen.getByText('OPENAI_API_KEY')).toBeInTheDocument())
    expect(screen.getByText('kube')).toBeInTheDocument()
    expect(screen.getByText('ab12cd34')).toBeInTheDocument()
  })
})
```

- [ ] **Step 2: Run it, confirm it fails (or that the old view does not match)**

Run: `pnpm -C web/app test src/views/Secrets.test.tsx`
Expected: FAIL until the view renders the table with provider + fingerprint columns.

- [ ] **Step 3: Implement the real `web/app/src/views/Secrets.tsx`**

Render: a create `<form>` (name `<input>`, value `<input type="password">`, Create button); on submit call `api.createSecret(name, value)` then clear the value field (never echo it); a table (Name, Provider, Mode, Version, Fingerprint) with Delete per row (toast on result). Loading `Skeleton`, empty `EmptyState` ("No secrets yet"). The value is write-only: after create only metadata shows.

(Implementer writes it using the existing `api.secrets`/`createSecret`/`deleteSecret`; the test pins the no-value-reveal list.)

- [ ] **Step 4: Ensure `/secrets` points at the view; run test, full suite, typecheck**

Run: `pnpm -C web/app test src/views/Secrets.test.tsx && pnpm -C web/app test && pnpm -C web/app typecheck`
Expected: PASS, clean.

- [ ] **Step 5: Commit**

```bash
git add web/app/src/views/Secrets.tsx web/app/src/nav/routes.tsx web/app/src/views/Secrets.test.tsx
git commit -s -m "feat(console): real Secrets view (write-only create, delete, metadata table)"
```

---

### Task 4: Usage, Audit, Templates, Billing views

**Files:**
- Create: `web/app/src/views/Usage.tsx`, `web/app/src/views/Audit.tsx`, `web/app/src/views/Templates.tsx`, `web/app/src/views/Billing.tsx`
- Modify: `web/app/src/nav/routes.tsx` (point the four routes at the views)
- Test: `web/app/src/views/ReadViews.test.tsx`

**Interfaces:**
- Consumes: `useUsage`, `useAudit`, `useTemplates`, `useBilling` (Task 1); `Skeleton`/`EmptyState`; `fmtBytes`.
- Produces: four views: Usage (totals + cost summary tiles or table), Audit (filterable event table: actor, action, target, time), Templates (table: name, image, description, updated), Billing (status, balance, spend, caps, ledger table + a Manage billing portal link).

- [ ] **Step 1: Write the failing test `web/app/src/views/ReadViews.test.tsx`**

```tsx
import { describe, it, expect, vi, beforeEach } from 'vitest'
import { waitFor, screen } from '@testing-library/react'
import { renderAt } from '../test/utils'
import type { Capabilities } from '../api'

const caps: Capabilities = { edition: 'community', billing: true, signup: false, teams: true, idp: 'oidc', orgSwitcher: false, secrets: { providers: ['kube'] }, proof: true, ownership: 'hosted' }

function mockFor(map: Record<string, unknown>) {
  vi.spyOn(globalThis, 'fetch').mockImplementation((input) => {
    const url = String(input).split('?')[0]
    const key = Object.keys(map).find((k) => url.endsWith(k))
    return Promise.resolve(new Response(JSON.stringify(key ? map[key] : {}), { status: 200, headers: { 'content-type': 'application/json' } }))
  })
}

beforeEach(() => {
  mockFor({
    '/console/capabilities': caps,
    '/console/audit': { org_id: 'o', events: [{ org_id: 'o', actor_id: 'alice', action: 'key.create', target: 'k1', detail: 'created api key mitos_live_ab12', at: '2026-06-25T08:00:00Z' }] },
    '/console/templates': { org_id: 'o', templates: [{ name: 'python-3.12', org_id: 'o', description: 'Python 3.12', image: 'ghcr.io/x/py:3.12', updated_at: '2026-06-01T00:00:00Z' }] },
    '/console/billing': { org_id: 'o', status: 'active', balance_cents: 5000, spend_cents: 1234, soft_cap_cents: 0, hard_cap_cents: 0, ledger_entries: [] },
    '/console/usage': { org_id: 'o', records: [], totals: { vcpu_seconds: 3600 }, cost: { total_cents: 1234 } },
  })
})

describe('read views', () => {
  it('audit shows an event', async () => {
    await renderAt('/audit', caps)
    await waitFor(() => expect(screen.getByText('key.create')).toBeInTheDocument())
    expect(screen.getByText('alice')).toBeInTheDocument()
  })
  it('templates shows a template', async () => {
    await renderAt('/templates', caps)
    await waitFor(() => expect(screen.getByText('python-3.12')).toBeInTheDocument())
  })
  it('billing shows status when capability is on', async () => {
    await renderAt('/billing', caps)
    await waitFor(() => expect(screen.getByText(/active/i)).toBeInTheDocument())
  })
})
```

- [ ] **Step 2: Run it, confirm it fails**

Run: `pnpm -C web/app test src/views/ReadViews.test.tsx`
Expected: FAIL (placeholders render, not the data).

- [ ] **Step 3: Implement the four views**

- `Usage.tsx`: render the `totals` and `cost` maps as a small set of `StatTile`s or a definition list (e.g. vCPU-seconds, cost). Honest empty state when totals are all zero.
- `Audit.tsx`: a filterable table (a text `<input aria-label="Filter audit">` filtering client-side over actor/action/target/detail) with columns Time, Actor, Action, Target, Detail. Empty state when no events.
- `Templates.tsx`: a table (Name, Image, Description, Updated). Empty state.
- `Billing.tsx`: status badge, balance and spend (cents to currency), cap info, a ledger table, and a `Manage billing` button that calls `api.billingPortal()` and opens the URL. Empty/loading states.

(Implementer writes all four; the test pins audit/templates/billing rendering.)

- [ ] **Step 4: Point the four routes at the views in `web/app/src/nav/routes.tsx`**

Import and set `/usage`, `/audit`, `/templates`, `/billing` elements to the real views. Keep `/billing`'s `when: (c) => c.billing` gate.

- [ ] **Step 5: Run the test, full suite, typecheck**

Run: `pnpm -C web/app test src/views/ReadViews.test.tsx && pnpm -C web/app test && pnpm -C web/app typecheck`
Expected: PASS, clean.

- [ ] **Step 6: Commit**

```bash
git add web/app/src/views/Usage.tsx web/app/src/views/Audit.tsx web/app/src/views/Templates.tsx web/app/src/views/Billing.tsx web/app/src/nav/routes.tsx web/app/src/views/ReadViews.test.tsx
git commit -s -m "feat(console): Usage, Audit, Templates, and Billing views over the live BFF"
```

---

### Task 5: Styles, a11y axe, final verification

**Files:**
- Modify: `web/packages/brand/src/base.css`
- Create: `web/app/src/views/B2bViews.a11y.test.tsx`

**Interfaces:**
- Produces: token-driven form/badge/ledger styles; an axe test asserting zero violations on Keys and Secrets (the form-bearing views).

- [ ] **Step 1: Append token-driven styles to `web/packages/brand/src/base.css`**

Add `.form-row` (label + control vertical rhythm), `input`/`select`/`textarea` base styling (token bg `var(--field-1)`, hairline border, focus ring `var(--cyan)`, 44px min height), `.badge` (status pill: `.badge-ok` green, `.badge-warn` amber), and `.ledger` (reuse `.tbl`). Token-driven, no raw hex. Add a `@media (max-width: 768px)` rule so forms stack and tables scroll.

- [ ] **Step 2: Write the axe a11y test `web/app/src/views/B2bViews.a11y.test.tsx`**

Mirror the existing a11y test pattern (`vitest-axe` + matchers). Render `/keys` and `/secrets`, wait for content, assert `expect(await axe(container)).toHaveNoViolations()` for each. Fix any real violation (form labels, button names); do not suppress rules.

```tsx
import { describe, it, expect, vi, beforeEach } from 'vitest'
import { waitFor, screen } from '@testing-library/react'
import { axe } from 'vitest-axe'
import * as matchers from 'vitest-axe/matchers'
import { renderAt } from '../test/utils'
import type { Capabilities } from '../api'

expect.extend(matchers)
const caps: Capabilities = { edition: 'community', billing: false, signup: false, teams: true, idp: 'oidc', orgSwitcher: false, secrets: { providers: ['kube'] }, proof: true, ownership: 'self-hosted' }
beforeEach(() => {
  vi.spyOn(globalThis, 'fetch').mockImplementation((input) => {
    const url = String(input)
    if (url.endsWith('/console/capabilities')) return Promise.resolve(new Response(JSON.stringify(caps), { status: 200, headers: { 'content-type': 'application/json' } }))
    if (url.endsWith('/console/keys')) return Promise.resolve(new Response(JSON.stringify({ org_id: 'o', keys: [] }), { status: 200, headers: { 'content-type': 'application/json' } }))
    if (url.endsWith('/console/secrets')) return Promise.resolve(new Response(JSON.stringify({ org_id: 'o', secrets: [] }), { status: 200, headers: { 'content-type': 'application/json' } }))
    return Promise.resolve(new Response(JSON.stringify({}), { status: 200, headers: { 'content-type': 'application/json' } }))
  })
})

describe('B2b views accessibility', () => {
  it('Keys has no axe violations', async () => {
    const { container } = await renderAt('/keys', caps)
    await waitFor(() => expect(screen.getByRole('button', { name: /create key/i })).toBeInTheDocument())
    expect(await axe(container)).toHaveNoViolations()
  })
})
```

- [ ] **Step 3: Run the a11y test; fix violations; final verification**

Run: `pnpm -C web/app test` (exit 0, all pass)
Run: `pnpm -C web/app typecheck` (clean)
Run: `pnpm -C web/app build` (succeeds)
Run: `grep -rnP '\xe2\x80\x94|\xe2\x80\x93' web/app/src/views web/app/src/ui/CopyOnce.tsx web/packages/brand/src/base.css` (empty)

- [ ] **Step 4: Commit**

```bash
git add web/packages/brand/src/base.css web/app/src/views/B2bViews.a11y.test.tsx
git commit -s -m "feat(console): form and badge styles plus B2b accessibility checks"
```

---

## Self-Review

**Spec coverage (section 4.3 IA):** API keys (Task 2, scoped/expiring/revocable/masked + create-once), Secrets (Task 3, write-only), Usage & cost (Task 4), Audit (Task 4, filterable list), Templates (Task 4), Billing hosted (Task 4, portal link). Covered. Members (roles) is B2c; Workspaces stays a placeholder (no BFF endpoint). 

**Secrets rule:** the raw key is shown once via `CopyOnce` (Task 1, used in Task 2) and never refetched; the masked prefix is all the list shows; secret values are write-only (Task 3). Enforced.

**Placeholder scan:** the honest Workspaces placeholder is intentional; every code step shows complete code (the four read views in Task 4 and the form views in Tasks 2-3 are described with their exact data sources and pinned by tests).

**Type consistency:** the api.ts shapes (Task 1) match the Go BFF JSON tags and are consumed by the hooks (Task 1) and views (Tasks 2-4); `CopyOnce` (Task 1) consumed by Task 2. No drift.
