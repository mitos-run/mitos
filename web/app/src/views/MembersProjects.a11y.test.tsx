// Axe accessibility audit for Members and Projects views.
// Renders each route and asserts zero axe violations.
// Uses vitest-axe (^0.1.0) which wraps axe-core and provides Vitest-compatible
// matchers. Import path: 'vitest-axe' for axe(), 'vitest-axe/matchers' for the
// custom expect matcher.
import { describe, it, expect, vi, beforeEach } from 'vitest'
import { waitFor, screen } from '@testing-library/react'
import { axe } from 'vitest-axe'
import * as matchers from 'vitest-axe/matchers'
import { renderAt } from '../test/utils'
import type { Capabilities } from '../api'

expect.extend(matchers)

const caps: Capabilities = {
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

const members = [
  { account_id: 'alice', org_id: 'o1', role: 'owner', created_at: '2026-01-01T00:00:00Z' },
  { account_id: 'bob', org_id: 'o1', role: 'member', created_at: '2026-03-15T00:00:00Z' },
]

const projects = [
  { id: 'p1', org_id: 'o1', name: 'alpha', description: 'first project', created_at: '2026-04-01T00:00:00Z' },
]

beforeEach(() => {
  vi.spyOn(globalThis, 'fetch').mockImplementation((input, init) => {
    const url = String(input).split('?')[0]
    const method = (init?.method ?? 'GET').toUpperCase()

    if (url.endsWith('/console/capabilities')) {
      return Promise.resolve(
        new Response(JSON.stringify(caps), {
          status: 200,
          headers: { 'content-type': 'application/json' },
        }),
      )
    }
    if (url.endsWith('/console/members') && method === 'GET') {
      return Promise.resolve(
        new Response(JSON.stringify({ org_id: 'o1', members }), {
          status: 200,
          headers: { 'content-type': 'application/json' },
        }),
      )
    }
    if (url.endsWith('/console/projects') && method === 'GET') {
      return Promise.resolve(
        new Response(JSON.stringify({ org_id: 'o1', projects }), {
          status: 200,
          headers: { 'content-type': 'application/json' },
        }),
      )
    }
    return Promise.resolve(
      new Response(JSON.stringify({}), {
        status: 200,
        headers: { 'content-type': 'application/json' },
      }),
    )
  })
})

describe('Members view accessibility', () => {
  it('has no axe violations once members have loaded', async () => {
    const { container } = await renderAt('/members', caps)
    await waitFor(() => expect(screen.getByText('alice')).toBeInTheDocument())
    expect(await axe(container)).toHaveNoViolations()
  })
})

describe('Projects view accessibility', () => {
  it('has no axe violations once projects have loaded', async () => {
    const { container } = await renderAt('/projects', caps)
    await waitFor(() => expect(screen.getByText('alpha')).toBeInTheDocument())
    expect(await axe(container)).toHaveNoViolations()
  })
})
