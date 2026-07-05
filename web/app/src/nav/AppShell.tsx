// The console chrome: a grouped sidebar (one section per NavGroup), the brand
// mark, the ownership / residency badge, and the routed content. Nav links use
// the router's intent preloading so hovering a link warms the target route.
//
// Responsive strategy: the sidebar is a persistent desktop panel (>= 769px)
// and a fixed off-canvas slide-over on mobile (<= 768px). Drawer open/closed
// state is explicit React state so it is testable in jsdom (which has no layout
// engine). CSS media queries handle the visual switching; state + ARIA wiring
// provide the accessible, keyboard-driven interface.
import { useState, useEffect, useRef } from 'react'
import { Link, Outlet, useRouterState } from '@tanstack/react-router'
import { useCapabilities } from '../data/query'
import { navRoutes, GROUP_ORDER, type NavGroupName, type RouteDef } from './routes'
import { CommandPalette } from './CommandPalette'
import { TopBar } from './TopBar'
import { LoadingScreen } from '../ui/LoadingScreen'
import type { Capabilities } from '../api'

export function AppShell() {
  const { data: caps } = useCapabilities()
  const [drawerOpen, setDrawerOpen] = useState(false)
  const [paletteOpen, setPaletteOpen] = useState(false)

  // Refs for focus management.
  const navRef = useRef<HTMLElement>(null)
  const menuButtonRef = useRef<HTMLButtonElement>(null)
  // Tracks whether the drawer was ever opened so we do not steal focus on initial mount.
  const wasOpen = useRef(false)

  // Close the drawer whenever the route changes (handles nav-link clicks on mobile).
  const pathname = useRouterState({ select: (s) => s.location.pathname })
  useEffect(() => {
    setDrawerOpen(false)
  }, [pathname])

  // Close the drawer on Escape; clean up the listener on unmount.
  useEffect(() => {
    function onKeyDown(e: KeyboardEvent) {
      if (e.key === 'Escape') {
        setDrawerOpen(false)
      }
    }
    window.addEventListener('keydown', onKeyDown)
    return () => window.removeEventListener('keydown', onKeyDown)
  }, [])

  // Toggle the command palette on Cmd-K / Ctrl-K.
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

  // Focus management: move focus into the nav when the drawer opens;
  // return focus to the menu button when it closes (but not on initial mount).
  useEffect(() => {
    if (drawerOpen) {
      wasOpen.current = true
      navRef.current?.focus()
    } else if (wasOpen.current) {
      menuButtonRef.current?.focus()
    }
  }, [drawerOpen])

  if (!caps) {
    return <LoadingScreen />
  }

  const routes = navRoutes(caps)

  return (
    <div className="app-shell-frame">
      <TopBar
        caps={caps}
        route={pathname}
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
          <VersionFooter caps={caps} />
        </nav>
        {/* minWidth: 0 overrides the flex item default of min-width:auto. Without
            it, a wide descendant (e.g. a .tbl forced to min-width:600px on a
            narrow viewport, or any long unbreakable token) sets main's own
            automatic minimum size and blows out the whole page width even
            though that descendant has its own overflow-x:auto wrapper: the
            wrapper only clips ITS children, it does not stop this flex item
            from being sized to fit them. This is the load-bearing fix that
            makes every "wrap the table in overflow-x:auto" pattern in the
            view files actually contain the scroll, instead of just adding a
            second (redundant) inner scrollbar next to a body that still
            scrolls horizontally. */}
        <main style={{ flex: 1, minWidth: 0, padding: 'var(--space-6)' }}>
          <Outlet />
        </main>
      </div>
      <CommandPalette caps={caps} open={paletteOpen} onOpenChange={setPaletteOpen} />
    </div>
  )
}

function NavSection({ group, routes }: { group: NavGroupName; routes: RouteDef[] }) {
  if (routes.length === 0) return null
  return (
    <div style={{ marginBottom: 'var(--space-5)' }}>
      <div
        className="t-dim"
        style={{
          fontSize: 'var(--step--1)',
          textTransform: 'uppercase',
          letterSpacing: '0.08em',
          marginBottom: 'var(--space-2)',
        }}
      >
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
  // The badge exists to reassure self-hosters about data residency. On the
  // hosted edition it would only restate where the user already knows they
  // are, so it renders nothing.
  if (caps.ownership !== 'self-hosted') return null
  return (
    <div className="card" style={{ marginTop: 'var(--space-6)', fontSize: 'var(--step--1)' }}>
      <div style={{ color: 'var(--cyan)' }}>Self-hosted</div>
      <div className="t-dim">Your data never leaves your infrastructure.</div>
    </div>
  )
}

// VersionFooter is a dim, mono, small line under the ownership badge: "mitos
// <version>". It hides entirely on an older server that has not yet been
// upgraded to advertise caps.version. Clicking copies the fuller
// "mitos <version> (<edition>)" string (useful when filing an issue), and
// shows a transient "Copied" state in an aria-live region so a screen reader
// user hears the confirmation too, not just a sighted user seeing the text
// swap. collectDiagnostics (lib/diagnostics.ts) also reads caps.version, so
// the same value that is shown and copied here is what feedback reports
// carry.
function VersionFooter({ caps }: { caps: Capabilities }) {
  const [copied, setCopied] = useState(false)
  const resetTimeout = useRef<ReturnType<typeof window.setTimeout> | null>(null)

  useEffect(() => {
    return () => {
      if (resetTimeout.current !== null) window.clearTimeout(resetTimeout.current)
    }
  }, [])

  if (!caps.version) return null

  async function handleCopy() {
    try {
      await navigator.clipboard.writeText(`mitos ${caps.version} (${caps.edition})`)
      setCopied(true)
      if (resetTimeout.current !== null) window.clearTimeout(resetTimeout.current)
      resetTimeout.current = window.setTimeout(() => setCopied(false), 1500)
    } catch {
      // Clipboard access can fail (permissions, insecure context); staying
      // silent keeps this a harmless no-op rather than surfacing an error for
      // what is a minor convenience.
    }
  }

  return (
    <button
      type="button"
      className="t-dim mono"
      onClick={() => void handleCopy()}
      aria-label={`Copy version: mitos ${caps.version} (${caps.edition})`}
      style={{
        display: 'block',
        marginTop: 'var(--space-3)',
        background: 'transparent',
        border: 'none',
        cursor: 'pointer',
        padding: 0,
        fontSize: 'var(--step--1)',
      }}
    >
      <span aria-live="polite">{copied ? 'Copied' : `mitos ${caps.version}`}</span>
    </button>
  )
}
