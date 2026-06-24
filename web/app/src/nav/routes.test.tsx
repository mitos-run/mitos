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
