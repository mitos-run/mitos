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
import { Division } from '@mitos/brand'
import { useCapabilities } from '../data/query'
import { navRoutes, GROUP_ORDER, type NavGroupName, type RouteDef } from './routes'
import { CommandPalette } from './CommandPalette'
import type { Capabilities } from '../api'

export function AppShell() {
  const { data: caps } = useCapabilities()
  const [drawerOpen, setDrawerOpen] = useState(false)

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
    return (
      <main style={{ padding: 'var(--space-6)' }}>
        <div className="t-dim">loading...</div>
      </main>
    )
  }

  const routes = navRoutes(caps)

  return (
    <div className="app-shell" style={{ display: 'flex', minHeight: '100vh', maxWidth: 'var(--maxw)', margin: '0 auto' }}>
      {/* Mobile top bar: hamburger + palette affordance */}
      <div className="top-bar">
        <button
          ref={menuButtonRef}
          className="menu-button"
          type="button"
          aria-label="Open navigation menu"
          aria-expanded={drawerOpen}
          aria-controls="primary-nav"
          onClick={() => setDrawerOpen((v) => !v)}
        >
          <MenuIcon />
          <span className="sr-only">Menu</span>
        </button>
        <div className="top-bar-brand" aria-hidden="true">
          <Division size={22} />
          <strong>mitos</strong>
        </div>
      </div>

      {/* Backdrop: dimmed scrim that closes the drawer on click (mobile only) */}
      {drawerOpen && (
        <div
          className="nav-backdrop"
          aria-hidden="true"
          onClick={() => setDrawerOpen(false)}
        />
      )}

      {/* Primary navigation: persistent sidebar on desktop, slide-over on mobile */}
      <nav
        ref={navRef}
        id="primary-nav"
        aria-label="Primary"
        tabIndex={-1}
        className={`nav-drawer${drawerOpen ? ' nav-drawer-open' : ''}`}
        style={{ width: 220, padding: 'var(--space-5)', borderRight: '1px solid var(--hairline)' }}
      >
        <div className="nav-brand" style={{ display: 'flex', alignItems: 'center', gap: 'var(--space-2)', marginBottom: 'var(--space-6)' }}>
          <Division size={28} />
          <strong>mitos</strong>
        </div>
        {GROUP_ORDER.map((group) => (
          <NavSection
            key={group}
            group={group}
            routes={routes.filter((r) => r.group === group)}
          />
        ))}
        <OwnershipBadge caps={caps} />
      </nav>

      <main style={{ flex: 1, padding: 'var(--space-6)' }}>
        <Outlet />
      </main>

      <CommandPalette caps={caps} />
    </div>
  )
}

// Simple three-line hamburger icon; purely presentational.
function MenuIcon() {
  return (
    <svg width="20" height="20" viewBox="0 0 20 20" fill="currentColor" aria-hidden="true" focusable="false">
      <rect x="2" y="4" width="16" height="2" rx="1" />
      <rect x="2" y="9" width="16" height="2" rx="1" />
      <rect x="2" y="14" width="16" height="2" rx="1" />
    </svg>
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
  const selfHosted = caps.ownership === 'self-hosted'
  return (
    <div className="card" style={{ marginTop: 'var(--space-6)', fontSize: 'var(--step--1)' }}>
      <div style={{ color: 'var(--cyan)' }}>{selfHosted ? 'Self-hosted' : 'Hosted by mitos'}</div>
      <div className="t-dim">
        {selfHosted
          ? 'Your data never leaves your infrastructure.'
          : 'Same engine and API; portable to self-host.'}
      </div>
    </div>
  )
}
