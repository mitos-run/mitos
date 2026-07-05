import { describe, it, expect } from 'vitest'
import { ROUTES, visibleRoutes, navRoutes, GROUP_ORDER } from './routes'
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

  it('always shows the overview home regardless of the proof capability', () => {
    // Overview is a real operational home (not just a proof screen), so it must
    // always be reachable even when proof is false.
    const visible = visibleRoutes({ ...base, proof: false })
    expect(visible.find((r) => r.path === '/')).toBeDefined()
    const visibleWithProof = visibleRoutes({ ...base, proof: true })
    expect(visibleWithProof.find((r) => r.path === '/')).toBeDefined()
  })

  it('groups Usage and Billing under a Billing group, not Govern', () => {
    expect(GROUP_ORDER).toContain('Billing')
    expect(ROUTES.find((r) => r.path === '/usage')?.group).toBe('Billing')
    expect(ROUTES.find((r) => r.path === '/billing')?.group).toBe('Billing')
  })

  it('keeps Settings reachable but out of the sidebar nav (moved to the account menu)', () => {
    // Account Settings lives in the top-bar account menu now, not a nav group.
    expect(GROUP_ORDER).not.toContain('Settings')
    expect(ROUTES.find((r) => r.path === '/settings')?.hidden).toBe(true)
    expect(navRoutes(base).find((r) => r.path === '/settings')).toBeUndefined()
    expect(visibleRoutes(base).find((r) => r.path === '/settings')).toBeDefined()
  })
})

describe('Operate nav group (instance-operator plane)', () => {
  const adminPaths = ['/admin', '/admin/orgs', '/admin/nodes', '/admin/waitlist']

  it('adds Operate to GROUP_ORDER after Billing', () => {
    expect(GROUP_ORDER).toContain('Operate')
    expect(GROUP_ORDER.indexOf('Operate')).toBeGreaterThan(GROUP_ORDER.indexOf('Billing'))
  })

  it('hides every /admin route when caps.admin is absent or false', () => {
    const visible = visibleRoutes(base)
    for (const path of adminPaths) {
      expect(visible.find((r) => r.path === path)).toBeUndefined()
    }
    const visibleFalse = visibleRoutes({ ...base, admin: false })
    for (const path of adminPaths) {
      expect(visibleFalse.find((r) => r.path === path)).toBeUndefined()
    }
  })

  it('shows every /admin route, grouped under Operate, when caps.admin is true', () => {
    const visible = visibleRoutes({ ...base, admin: true })
    for (const path of adminPaths) {
      const route = visible.find((r) => r.path === path)
      expect(route).toBeDefined()
      expect(route?.group).toBe('Operate')
    }
  })

  it('never shows an /admin route in the nav for a non-admin caller', () => {
    const nav = navRoutes(base)
    for (const path of adminPaths) {
      expect(nav.find((r) => r.path === path)).toBeUndefined()
    }
  })
})
