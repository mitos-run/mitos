// Axe accessibility audit for the Keys view: no violations with a populated
// key table, create form, and revoke controls.
// Uses vitest-axe (^0.1.0) which wraps axe-core and provides Vitest-compatible
// matchers. Import path: 'vitest-axe' for axe(), 'vitest-axe/matchers' for the
// custom expect matcher.
import { describe, it, expect, vi, beforeEach } from 'vitest'
import { screen } from '@testing-library/react'
import { axe } from 'vitest-axe'
import * as matchers from 'vitest-axe/matchers'
import { renderAt } from '../test/utils'
import type { Capabilities } from '../api'

expect.extend(matchers)

const caps: Capabilities = {
  edition: 'community', billing: true, signup: false, teams: true, idp: 'oidc',
  orgSwitcher: false, secrets: { providers: ['kube'] }, proof: true, ownership: 'self-hosted',
}

const baseKey = {
  id: 'k1',
  name: 'ci-bot',
  prefix: 'mitos_live_ab12',
  scopes: ['sandboxes'],
  created_at: '2026-01-01T00:00:00Z',
  revoked: false,
}

const revokedKey = {
  id: 'k2',
  name: 'old-key',
  prefix: 'mitos_live_zz99',
  scopes: ['read'],
  created_at: '2025-12-01T00:00:00Z',
  revoked: true,
  revoked_at: '2026-01-10T00:00:00Z',
}

beforeEach(() => {
  vi.spyOn(globalThis, 'fetch').mockImplementation((input) => {
    const url = String(input)
    if (url.endsWith('/console/capabilities')) {
      return Promise.resolve(new Response(JSON.stringify(caps), { status: 200, headers: { 'content-type': 'application/json' } }))
    }
    if (url.endsWith('/console/keys') && !url.includes('revoke')) {
      return Promise.resolve(
        new Response(JSON.stringify({ keys: [baseKey, revokedKey] }), {
          status: 200,
          headers: { 'content-type': 'application/json' },
        }),
      )
    }
    return Promise.resolve(new Response(JSON.stringify({}), { status: 200, headers: { 'content-type': 'application/json' } }))
  })
})

describe('Keys accessibility', () => {
  it('has no axe violations', async () => {
    const { container } = await renderAt('/keys', caps)
    // Wait for the query to resolve so axe audits the populated table, not the skeleton.
    await screen.findByRole('heading', { name: /api keys/i })
    expect(await axe(container)).toHaveNoViolations()
  })
})
