# Console shell workstream 2: global top bar Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development. Steps use checkbox (`- [ ]`). Do NOT invoke finishing-a-development-branch; implement, test, commit, report.

**Goal:** A persistent global top bar across the console that continues the marketing site nav (translucent 64px bar, brand, magenta glow), carrying a discoverable search trigger (opens the existing command palette), a help link, and an account menu. This makes the website to app handoff seamless and gives the app the global-context bar that world-class dashboards have.

**Architecture:** A new `TopBar` assembled from small components (`SearchTrigger`, `AccountMenu`) and the brand. `AppShell` owns the command-palette open state and the mobile drawer state and passes them down; the `CommandPalette` becomes a controlled component so the search trigger and `Cmd-K` both open it. No BFF changes. The interactive org switcher is NOT in this workstream (it needs a membership-constrained BFF change and isolation tests; it is workstream 2b).

**Tech Stack:** React 18 + TypeScript (strict), TanStack Router + Query, Vitest + vitest-axe, the `@mitos/brand` package, CSS in `web/packages/brand/src/base.css`.

## Global Constraints

- **Punctuation (strict):** no em (U+2014) or en (U+2013) dashes anywhere (source, comments, copy, CSS). Only `.` `,` `;` `:` and ASCII `-`. Verify each commit with the grep in the final task.
- **Commits:** conventional + DCO (`git commit -s`). **Staging:** explicit paths only; never `git add -A`.
- **Continuity:** the top bar mirrors the site nav in `website/src/layouts/Site.astro`: 64px height, translucent field background (`rgba(4,5,10,.55)` + `backdrop-filter: blur(12px)`), a hairline bottom border that appears on scroll, the capital "Mitos" wordmark, the mark with its magenta glow.
- **Honesty:** no dead controls. The account menu contains only working actions: the caller name/email, an "Account settings" link to `/settings`, and "Sign out" (the existing `revokeAll` session mechanism). No org switcher, no theme toggle in this workstream.
- **Accessibility (spec 4.6):** the account menu is a real menu button (`aria-haspopup`, `aria-expanded`), closes on Escape and outside-click, returns focus to its trigger; the search trigger is a labelled button; axe reports zero violations. Responsive to mobile.
- **Quality floor:** TypeScript strict clean; `pnpm -C web/app test` exits 0; `pnpm -C web/app typecheck` clean; `pnpm -C web/app build` succeeds.

## File Structure

- `web/app/src/nav/CommandPalette.tsx` (modify) - become controlled: props `{ caps, open, onOpenChange }`; remove the internal `Cmd-K` listener (AppShell owns it); keep Escape and outside-click close via `onOpenChange(false)`.
- `web/app/src/nav/SearchTrigger.tsx` (create) - a button styled as a search input that calls `onClick`; shows "Search" and a `Cmd-K` hint.
- `web/app/src/nav/AccountMenu.tsx` (create) - avatar button + dropdown (name/email, Account settings link, Sign out).
- `web/app/src/nav/TopBar.tsx` (create) - assembles brand + hamburger (mobile) + SearchTrigger + Help link + AccountMenu.
- `web/app/src/nav/AppShell.tsx` (modify) - own `paletteOpen` and the `Cmd-K` listener; render `TopBar`; remove the old mobile `.top-bar` brand block and the sidebar `nav-brand` block.
- `web/app/src/data/account-settings.ts` (modify) - add `useSignOut` (wraps `api.revokeAllSessions`) if not reused from existing `useRevokeAllSessions`.
- `web/packages/brand/src/base.css` (modify) - top bar, search trigger, account menu, responsive styles.
- Tests: `CommandPalette.test.tsx` (modify), `SearchTrigger.test.tsx`, `AccountMenu.test.tsx`, `TopBar.test.tsx`, `TopBar.a11y.test.tsx` (create), `AppShell.test.tsx` (modify if it asserts the old brand blocks).

---

### Task 1: Make the command palette controlled, owned by AppShell

**Files:**
- Modify: `web/app/src/nav/CommandPalette.tsx`
- Modify: `web/app/src/nav/AppShell.tsx`
- Test: `web/app/src/nav/CommandPalette.test.tsx`

Read `CommandPalette.tsx` and `AppShell.tsx` first. The palette currently owns its own `open` state and a `Cmd-K` window listener. We invert that: AppShell owns the state and the shortcut; the palette renders when `open` is true and reports closes via `onOpenChange`.

**Interfaces:**
- Produces: `CommandPalette(props: { caps: Capabilities; open: boolean; onOpenChange: (open: boolean) => void })`. When `open` is false it renders null. Selecting a route or pressing Escape or clicking the backdrop calls `onOpenChange(false)`.
- AppShell holds `const [paletteOpen, setPaletteOpen] = useState(false)` and a `Cmd-K`/`Ctrl-K` window listener that toggles it.

- [ ] **Step 1: Update the palette test** `web/app/src/nav/CommandPalette.test.tsx` to drive the controlled API.

```tsx
import { describe, it, expect, vi } from 'vitest'
import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { CommandPalette, fuzzyMatch } from './CommandPalette'
import type { Capabilities } from '../api'

const caps: Capabilities = {
  edition: 'community', billing: false, signup: false, teams: true, idp: 'oidc',
  orgSwitcher: false, secrets: { providers: ['kube'] }, proof: true, ownership: 'self-hosted',
}

vi.mock('@tanstack/react-router', () => ({ useNavigate: () => () => {} }))

describe('CommandPalette (controlled)', () => {
  it('renders nothing when open is false', () => {
    const { container } = render(<CommandPalette caps={caps} open={false} onOpenChange={() => {}} />)
    expect(container.firstChild).toBeNull()
  })

  it('renders the input when open is true and reports close on Escape', async () => {
    const onOpenChange = vi.fn()
    render(<CommandPalette caps={caps} open onOpenChange={onOpenChange} />)
    const input = screen.getByLabelText('Command palette input')
    expect(input).toBeInTheDocument()
    await userEvent.type(input, '{Escape}')
    expect(onOpenChange).toHaveBeenCalledWith(false)
  })

  it('fuzzyMatch still matches subsequences', () => {
    expect(fuzzyMatch('ovw', 'Overview')).toBe(true)
    expect(fuzzyMatch('zzz', 'Overview')).toBe(false)
  })
})
```

- [ ] **Step 2: Run it, confirm it fails**

Run: `pnpm -C web/app test src/nav/CommandPalette.test.tsx`
Expected: FAIL (palette is not yet controlled; it ignores the `open` prop).

- [ ] **Step 3: Make the palette controlled.** Replace the internal `open` state and the `Cmd-K` listener. Keep `query` state and `fuzzyMatch`.

```tsx
export function CommandPalette({ caps, open, onOpenChange }: { caps: Capabilities; open: boolean; onOpenChange: (open: boolean) => void }) {
  const [query, setQuery] = useState('')
  const navigate = useNavigate()
  const routes = useMemo(() => visibleRoutes(caps), [caps])
  const results = useMemo(() => routes.filter((r) => fuzzyMatch(query, r.label)), [routes, query])

  // Reset the filter each time the palette opens.
  useEffect(() => { if (open) setQuery('') }, [open])

  // Escape closes (the input has focus, so a window listener is enough here).
  useEffect(() => {
    if (!open) return
    function onKey(e: KeyboardEvent) { if (e.key === 'Escape') onOpenChange(false) }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [open, onOpenChange])

  if (!open) return null

  function go(path: string) { onOpenChange(false); setQuery(''); void navigate({ to: path }) }

  return (
    <div role="dialog" aria-label="Command palette" className="palette-backdrop" onClick={() => onOpenChange(false)}>
      <div className="palette" onClick={(e) => e.stopPropagation()}>
        <input
          autoFocus
          aria-label="Command palette input"
          placeholder="Jump to..."
          value={query}
          onChange={(e) => setQuery(e.target.value)}
          onKeyDown={(e) => { if (e.key === 'Enter' && results[0]) go(results[0].path) }}
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

- [ ] **Step 4: Wire AppShell to own the state + shortcut.** In `AppShell`, add `const [paletteOpen, setPaletteOpen] = useState(false)`. Add a window listener that toggles it on `Cmd-K`/`Ctrl-K`:

```tsx
useEffect(() => {
  function onKey(e: KeyboardEvent) {
    if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === 'k') {
      e.preventDefault()
      setPaletteOpen((v) => !v)
    }
  }
  window.addEventListener('keydown', onKey)
  return () => window.removeEventListener('keydown', onKey)
}, [])
```

Change the render to `<CommandPalette caps={caps} open={paletteOpen} onOpenChange={setPaletteOpen} />`.

- [ ] **Step 5: Run the palette test + full suite + typecheck**

Run: `pnpm -C web/app test src/nav/CommandPalette.test.tsx && pnpm -C web/app test && pnpm -C web/app typecheck`
Expected: PASS, clean. (If `AppShell.test.tsx` asserted the old `Cmd-K`-from-palette behavior, update it to open via `setPaletteOpen` or the trigger from Task 2.)

- [ ] **Step 6: Commit**

```bash
git add web/app/src/nav/CommandPalette.tsx web/app/src/nav/AppShell.tsx web/app/src/nav/CommandPalette.test.tsx
git commit -s -m "refactor(console): make the command palette a controlled component"
```

---

### Task 2: SearchTrigger button (discoverable search)

**Files:**
- Create: `web/app/src/nav/SearchTrigger.tsx`
- Test: `web/app/src/nav/SearchTrigger.test.tsx`

**Interfaces:**
- Produces: `SearchTrigger(props: { onClick: () => void })` - a `<button type="button" className="search-trigger" aria-label="Search (Cmd K)">` containing a search-glyph, the word "Search", and a `<kbd>` hint. Clicking it calls `onClick`.

- [ ] **Step 1: Write the failing test** `web/app/src/nav/SearchTrigger.test.tsx`

```tsx
import { describe, it, expect, vi } from 'vitest'
import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { SearchTrigger } from './SearchTrigger'

describe('SearchTrigger', () => {
  it('renders a labelled button and calls onClick', async () => {
    const onClick = vi.fn()
    render(<SearchTrigger onClick={onClick} />)
    const btn = screen.getByRole('button', { name: /search/i })
    await userEvent.click(btn)
    expect(onClick).toHaveBeenCalledTimes(1)
  })
})
```

- [ ] **Step 2: Run it, confirm it fails**

Run: `pnpm -C web/app test src/nav/SearchTrigger.test.tsx`
Expected: FAIL (module not found).

- [ ] **Step 3: Implement** `web/app/src/nav/SearchTrigger.tsx`

```tsx
// A button styled as a search input. Clicking it (or Cmd-K, handled by AppShell)
// opens the command palette. Making search visible is what makes the palette
// discoverable to new users.
export function SearchTrigger({ onClick }: { onClick: () => void }) {
  return (
    <button type="button" className="search-trigger" aria-label="Search (Cmd K)" onClick={onClick}>
      <svg width="15" height="15" viewBox="0 0 16 16" fill="none" aria-hidden="true" focusable="false">
        <circle cx="7" cy="7" r="5" stroke="currentColor" strokeWidth="1.5" />
        <path d="M11 11l3.5 3.5" stroke="currentColor" strokeWidth="1.5" strokeLinecap="round" />
      </svg>
      <span className="search-trigger-label">Search</span>
      <kbd className="search-trigger-kbd">Cmd K</kbd>
    </button>
  )
}
```

- [ ] **Step 4: Run the test, full suite, typecheck**

Run: `pnpm -C web/app test src/nav/SearchTrigger.test.tsx && pnpm -C web/app typecheck`
Expected: PASS, clean.

- [ ] **Step 5: Commit**

```bash
git add web/app/src/nav/SearchTrigger.tsx web/app/src/nav/SearchTrigger.test.tsx
git commit -s -m "feat(console): discoverable search trigger button"
```

---

### Task 3: AccountMenu (avatar + dropdown)

**Files:**
- Create: `web/app/src/nav/AccountMenu.tsx`
- Modify: `web/app/src/data/account-settings.ts`
- Test: `web/app/src/nav/AccountMenu.test.tsx`

Read `web/app/src/data/account-settings.ts` first. It already has `useAccount()` returning `AccountView` ({ display_name, email, ... }) and a revoke-all-sessions mutation used by Settings ("sign out everywhere"). Reuse the existing revoke-all hook; if it is not exported, add `useSignOut` that wraps `api.revokeAllSessions()` and on success reloads to the login screen via `window.location.assign('/')`.

**Interfaces:**
- Consumes: `useAccount()` -> `{ data?: AccountView }`; the revoke-all mutation.
- Produces: `AccountMenu()` - a menu button showing the caller initial; on open, a dropdown with the display name + email, an "Account settings" router `Link` to `/settings`, and a "Sign out" button. `aria-haspopup="menu"`, `aria-expanded` reflects open; Escape and outside-click close; focus returns to the button on close.

- [ ] **Step 1: Write the failing test** `web/app/src/nav/AccountMenu.test.tsx` (mock the router `Link` and `useAccount`; assert the button shows the initial, opening reveals name + email + the two actions, Escape closes).

```tsx
import { describe, it, expect, vi } from 'vitest'
import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { AccountMenu } from './AccountMenu'

vi.mock('@tanstack/react-router', () => ({ Link: (p: any) => <a href={p.to}>{p.children}</a> }))
vi.mock('../data/account-settings', () => ({
  useAccount: () => ({ data: { account_id: 'a1', email: 'alice@acme.dev', display_name: 'Alice Anderson', timezone: 'UTC', locale: 'en', memberships: [] } }),
  useSignOut: () => ({ mutate: vi.fn(), isPending: false }),
}))

describe('AccountMenu', () => {
  it('opens to show identity and actions, closes on Escape', async () => {
    render(<AccountMenu />)
    const btn = screen.getByRole('button', { name: /account menu/i })
    expect(btn).toHaveAttribute('aria-expanded', 'false')
    await userEvent.click(btn)
    expect(btn).toHaveAttribute('aria-expanded', 'true')
    expect(screen.getByText('alice@acme.dev')).toBeInTheDocument()
    expect(screen.getByText(/account settings/i)).toBeInTheDocument()
    expect(screen.getByText(/sign out/i)).toBeInTheDocument()
    await userEvent.keyboard('{Escape}')
    expect(btn).toHaveAttribute('aria-expanded', 'false')
  })
})
```

- [ ] **Step 2: Run it, confirm it fails**

Run: `pnpm -C web/app test src/nav/AccountMenu.test.tsx`
Expected: FAIL (module not found).

- [ ] **Step 3: Add the sign-out hook** to `web/app/src/data/account-settings.ts` (reuse the revoke-all mutation if already present; otherwise add):

```tsx
export function useSignOut() {
  return useMutation({
    mutationFn: () => api.revokeAllSessions(),
    onSuccess: () => { window.location.assign('/') },
  })
}
```

(If `api.revokeAllSessions` does not exist, use the existing revoke-all method name found in `api.ts`; match it exactly. Do NOT invent a new endpoint.)

- [ ] **Step 4: Implement** `web/app/src/nav/AccountMenu.tsx`

```tsx
// The account menu: caller identity, a link to account settings, and sign out.
// A real menu button: aria-haspopup, aria-expanded, Escape and outside-click
// close, focus returns to the trigger.
import { useEffect, useRef, useState } from 'react'
import { Link } from '@tanstack/react-router'
import { useAccount, useSignOut } from '../data/account-settings'

export function AccountMenu() {
  const { data: account } = useAccount()
  const signOut = useSignOut()
  const [open, setOpen] = useState(false)
  const btnRef = useRef<HTMLButtonElement>(null)
  const popRef = useRef<HTMLDivElement>(null)

  useEffect(() => {
    if (!open) return
    function onDocClick(e: MouseEvent) {
      if (!popRef.current?.contains(e.target as Node) && !btnRef.current?.contains(e.target as Node)) setOpen(false)
    }
    function onKey(e: KeyboardEvent) { if (e.key === 'Escape') { setOpen(false); btnRef.current?.focus() } }
    document.addEventListener('mousedown', onDocClick)
    document.addEventListener('keydown', onKey)
    return () => { document.removeEventListener('mousedown', onDocClick); document.removeEventListener('keydown', onKey) }
  }, [open])

  const initial = (account?.display_name || account?.email || '?').trim().charAt(0).toUpperCase()

  return (
    <div className="account-menu">
      <button
        ref={btnRef}
        type="button"
        className="account-avatar"
        aria-haspopup="menu"
        aria-expanded={open}
        aria-label="Account menu"
        onClick={() => setOpen((v) => !v)}
      >
        {initial}
      </button>
      {open && (
        <div ref={popRef} role="menu" className="account-pop">
          <div className="account-id">
            <div className="account-name">{account?.display_name}</div>
            <div className="t-dim">{account?.email}</div>
          </div>
          <Link role="menuitem" to="/settings" className="account-item" onClick={() => setOpen(false)}>Account settings</Link>
          <button role="menuitem" type="button" className="account-item account-signout" disabled={signOut.isPending} onClick={() => signOut.mutate()}>Sign out</button>
        </div>
      )}
    </div>
  )
}
```

- [ ] **Step 5: Run the test, full suite, typecheck**

Run: `pnpm -C web/app test src/nav/AccountMenu.test.tsx && pnpm -C web/app test && pnpm -C web/app typecheck`
Expected: PASS, clean.

- [ ] **Step 6: Commit**

```bash
git add web/app/src/nav/AccountMenu.tsx web/app/src/data/account-settings.ts web/app/src/nav/AccountMenu.test.tsx
git commit -s -m "feat(console): account menu with identity, settings link, and sign out"
```

---

### Task 4: TopBar assembly + AppShell integration

**Files:**
- Create: `web/app/src/nav/TopBar.tsx`
- Modify: `web/app/src/nav/AppShell.tsx`
- Test: `web/app/src/nav/TopBar.test.tsx`, modify `web/app/src/nav/AppShell.test.tsx` if needed

Read the current `AppShell.tsx` render (the `.top-bar` mobile block and the sidebar `.nav-brand` block). The TopBar replaces both as the single brand home and adds the hamburger (mobile), the search trigger, a help link, and the account menu.

**Interfaces:**
- Consumes: `SearchTrigger`, `AccountMenu`, `Mark` from `@mitos/brand`.
- Produces: `TopBar(props: { onSearch: () => void; onToggleDrawer: () => void; drawerOpen: boolean; menuButtonRef: React.RefObject<HTMLButtonElement> })`. Layout: hamburger (mobile-only via CSS) + brand `Link to="/"` (Mark glow + "Mitos") on the left; a spacer; `SearchTrigger`; a Help link (`<a href="/docs">` opening docs); `AccountMenu` on the right.

- [ ] **Step 1: Write the failing test** `web/app/src/nav/TopBar.test.tsx`

```tsx
import { describe, it, expect, vi } from 'vitest'
import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { createRef } from 'react'
import { TopBar } from './TopBar'

vi.mock('@tanstack/react-router', () => ({ Link: (p: any) => <a href={p.to}>{p.children}</a> }))
vi.mock('../data/account-settings', () => ({
  useAccount: () => ({ data: { display_name: 'Alice', email: 'alice@acme.dev', memberships: [] } }),
  useSignOut: () => ({ mutate: vi.fn(), isPending: false }),
}))

describe('TopBar', () => {
  it('renders the brand, a search trigger that fires onSearch, and the account menu', async () => {
    const onSearch = vi.fn()
    render(<TopBar onSearch={onSearch} onToggleDrawer={() => {}} drawerOpen={false} menuButtonRef={createRef()} />)
    expect(screen.getByText('Mitos')).toBeInTheDocument()
    await userEvent.click(screen.getByRole('button', { name: /search/i }))
    expect(onSearch).toHaveBeenCalled()
    expect(screen.getByRole('button', { name: /account menu/i })).toBeInTheDocument()
  })
})
```

- [ ] **Step 2: Run it, confirm it fails**

Run: `pnpm -C web/app test src/nav/TopBar.test.tsx`
Expected: FAIL (module not found).

- [ ] **Step 3: Implement** `web/app/src/nav/TopBar.tsx`

```tsx
// The global top bar: the single brand home, the mobile drawer toggle, a
// discoverable search trigger, a help link, and the account menu. Styled to
// continue the marketing site nav (translucent 64px bar) so the website to app
// handoff is seamless.
import type { RefObject } from 'react'
import { Link } from '@tanstack/react-router'
import { Mark } from '@mitos/brand'
import { SearchTrigger } from './SearchTrigger'
import { AccountMenu } from './AccountMenu'

export function TopBar({ onSearch, onToggleDrawer, drawerOpen, menuButtonRef }: {
  onSearch: () => void
  onToggleDrawer: () => void
  drawerOpen: boolean
  menuButtonRef: RefObject<HTMLButtonElement>
}) {
  return (
    <header className="topbar">
      <button
        ref={menuButtonRef}
        className="menu-button"
        type="button"
        aria-label="Open navigation menu"
        aria-expanded={drawerOpen}
        aria-controls="primary-nav"
        onClick={onToggleDrawer}
      >
        <svg width="20" height="20" viewBox="0 0 20 20" fill="currentColor" aria-hidden="true" focusable="false">
          <rect x="2" y="4" width="16" height="2" rx="1" /><rect x="2" y="9" width="16" height="2" rx="1" /><rect x="2" y="14" width="16" height="2" rx="1" />
        </svg>
        <span className="sr-only">Menu</span>
      </button>
      <Link to="/" className="topbar-brand" aria-label="Mitos home">
        <Mark size={22} glow />
        <strong>Mitos</strong>
      </Link>
      <div className="topbar-spacer" />
      <SearchTrigger onClick={onSearch} />
      <a className="topbar-help" href="/docs" target="_blank" rel="noreferrer">Help</a>
      <AccountMenu />
    </header>
  )
}
```

- [ ] **Step 4: Integrate into AppShell.** Replace the old `.top-bar` mobile block and the sidebar `.nav-brand` block with `<TopBar .../>`. Wrap the shell so the top bar spans full width above the sidebar+main row. Keep the existing drawer state, refs, and focus management; pass `menuButtonRef`, `onToggleDrawer={() => setDrawerOpen((v) => !v)}`, `drawerOpen`, and `onSearch={() => setPaletteOpen(true)}`.

```tsx
return (
  <div className="app-shell-frame">
    <TopBar
      onSearch={() => setPaletteOpen(true)}
      onToggleDrawer={() => setDrawerOpen((v) => !v)}
      drawerOpen={drawerOpen}
      menuButtonRef={menuButtonRef}
    />
    <div className="app-shell" style={{ display: 'flex', minHeight: 'calc(100vh - 64px)', maxWidth: 'var(--maxw)', margin: '0 auto' }}>
      {drawerOpen && <div className="nav-backdrop" aria-hidden="true" onClick={() => setDrawerOpen(false)} />}
      <nav ref={navRef} id="primary-nav" aria-label="Primary" tabIndex={-1}
           className={`nav-drawer${drawerOpen ? ' nav-drawer-open' : ''}`}
           style={{ width: 220, padding: 'var(--space-5)', borderRight: '1px solid var(--hairline)' }}>
        {GROUP_ORDER.map((group) => (
          <NavSection key={group} group={group} routes={routes.filter((r) => r.group === group)} />
        ))}
        <OwnershipBadge caps={caps} />
      </nav>
      <main style={{ flex: 1, padding: 'var(--space-6)' }}>
        <Outlet />
      </main>
    </div>
    <CommandPalette caps={caps} open={paletteOpen} onOpenChange={setPaletteOpen} />
  </div>
)
```

Remove the now-unused inline `MenuIcon` if it is no longer referenced (the hamburger SVG now lives in TopBar). Keep `NavSection` and `OwnershipBadge`.

- [ ] **Step 5: Run TopBar test + full suite + typecheck.** Fix any `AppShell.test.tsx` assertion that referenced the removed `.top-bar`/`.nav-brand` brand blocks (the brand now renders once, in the TopBar; a test asserting two "Mitos" wordmarks must drop to one).

Run: `pnpm -C web/app test && pnpm -C web/app typecheck`
Expected: PASS, clean.

- [ ] **Step 6: Commit**

```bash
git add web/app/src/nav/TopBar.tsx web/app/src/nav/AppShell.tsx web/app/src/nav/TopBar.test.tsx
git commit -s -m "feat(console): assemble the global top bar and integrate it into the shell"
```

---

### Task 5: Styles, a11y, final verification

**Files:**
- Modify: `web/packages/brand/src/base.css`
- Create: `web/app/src/nav/TopBar.a11y.test.tsx`

Read the site nav styles in `website/src/layouts/Site.astro` (the `.nav`, `.nav.scrolled`, `.brand`, `.brand-mark` rules) and the existing `.top-bar`, `.menu-button`, `.palette` rules in `base.css` so the new bar reuses tokens and matches the site.

- [ ] **Step 1: Append token-driven styles** for `.topbar`, `.topbar-brand`, `.topbar-spacer`, `.search-trigger` (+ `-label`, `-kbd`), `.topbar-help`, `.account-menu`, `.account-avatar`, `.account-pop`, `.account-id`, `.account-name`, `.account-item`, `.account-signout`. Match the site: `.topbar { position: sticky; top: 0; z-index: 100; height: 64px; display: flex; align-items: center; gap: var(--space-4); padding: 0 var(--space-5); backdrop-filter: blur(12px); background: rgba(4,5,10,.55); border-bottom: 1px solid var(--hairline); }`. The search trigger reads as a quiet input (hairline border, `--field-1` fill, dim text, the `kbd` in a faint pill). The account dropdown is a `.card`-elevation popover, right-aligned. No raw hex outside the one translucent field background already used by the site (`rgba(4,5,10,.55)`); prefer tokens. Add a mobile rule: hide `.topbar-help` and the `.search-trigger-label`+`.search-trigger-kbd` (keep the search glyph) under 768px; reveal the `.menu-button` (it is hidden on desktop). Remove or retire the old `.top-bar` mobile-brand rules now that TopBar owns the brand.

- [ ] **Step 2: Write the axe a11y test** `web/app/src/nav/TopBar.a11y.test.tsx` (render `TopBar` with the same mocks as `TopBar.test.tsx`, open the account menu, assert `toHaveNoViolations()`). Fix any real violation.

```tsx
import { describe, it, expect, vi } from 'vitest'
import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { createRef } from 'react'
import { axe } from 'vitest-axe'
import * as matchers from 'vitest-axe/matchers'
import { TopBar } from './TopBar'

expect.extend(matchers)
vi.mock('@tanstack/react-router', () => ({ Link: (p: any) => <a href={p.to}>{p.children}</a> }))
vi.mock('../data/account-settings', () => ({
  useAccount: () => ({ data: { display_name: 'Alice', email: 'alice@acme.dev', memberships: [] } }),
  useSignOut: () => ({ mutate: vi.fn(), isPending: false }),
}))

describe('TopBar a11y', () => {
  it('has no axe violations with the account menu open', async () => {
    const { container } = render(<TopBar onSearch={() => {}} onToggleDrawer={() => {}} drawerOpen={false} menuButtonRef={createRef()} />)
    await userEvent.click(screen.getByRole('button', { name: /account menu/i }))
    expect(await axe(container)).toHaveNoViolations()
  })
})
```

- [ ] **Step 3: Final verification**

Run: `pnpm -C web/app test` (exit 0) ; `pnpm -C web/app typecheck` (clean) ; `pnpm -C web/app build` (succeeds)
Run: `grep -rnP '\xe2\x80\x94|\xe2\x80\x93' web/app/src/nav/TopBar.tsx web/app/src/nav/AccountMenu.tsx web/app/src/nav/SearchTrigger.tsx web/packages/brand/src/base.css` (must be empty)

- [ ] **Step 4: Commit**

```bash
git add web/packages/brand/src/base.css web/app/src/nav/TopBar.a11y.test.tsx
git commit -s -m "feat(console): top bar styles and accessibility checks"
```

---

## Self-Review

**Spec coverage (workstream 2 in the shell design):** global top bar continuing the site nav (Tasks 4, 5); discoverable search opening the command palette (Tasks 1, 2); help link (Task 4); account menu with identity + account settings + sign out (Task 3). The org switcher is explicitly deferred to workstream 2b (it needs a membership-constrained BFF change + isolation tests); noted in the plan header and Global Constraints. Account-settings-moves-to-the-menu is additive here; removing the Settings sidebar group is workstream 4.

**Honesty:** no dead controls. Search opens a real palette; Help opens real docs; the account menu links to a real settings route and uses the real revoke-all session mechanism for sign out. No org switcher (would be a non-functional control today) and no theme toggle (the app is dark-only; appearance prefs live in Settings).

**Type consistency:** `CommandPalette` props `{ caps, open, onOpenChange }` are produced in Task 1 and consumed by AppShell in Task 4; `SearchTrigger({ onClick })` (Task 2) is consumed by TopBar (Task 4); `AccountMenu()` + `useSignOut` (Task 3) are consumed by TopBar (Task 4). `TopBar` props match AppShell's call site in Task 4. No drift.
