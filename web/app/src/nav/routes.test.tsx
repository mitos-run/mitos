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

  it('hides the overview home when proof is false', () => {
    const visible = visibleRoutes({ ...base, proof: false })
    expect(visible.find((r) => r.path === '/')).toBeUndefined()
  })
})
