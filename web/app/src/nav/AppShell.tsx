// The console chrome: a grouped sidebar (one section per NavGroup), the brand
// mark, the ownership / residency badge, and the routed content. Nav links use
// the router's intent preloading so hovering a link warms the target route.
import { Link, Outlet } from '@tanstack/react-router'
import { Division } from '@mitos/brand'
import { useCapabilities } from '../data/query'
import { visibleRoutes, GROUP_ORDER, type NavGroupName, type RouteDef } from './routes'
import { CommandPalette } from './CommandPalette'
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
      <CommandPalette caps={caps} />
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
