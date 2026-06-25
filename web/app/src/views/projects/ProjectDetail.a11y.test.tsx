// Axe accessibility audit for the ProjectDetail view (/projects/$id):
// members table with labelled headers and revoke buttons, plus the assign
// form with labelled account input and role select. Zero violations required.
// Uses vitest-axe (^0.1.0) which wraps axe-core and provides Vitest-compatible
// matchers. Import path: 'vitest-axe' for axe(), 'vitest-axe/matchers' for the
// custom expect matcher.
import { describe, it, expect, vi, beforeEach } from 'vitest'
import { waitFor, screen } from '@testing-library/react'
import { axe } from 'vitest-axe'
import * as matchers from 'vitest-axe/matchers'
import { renderAt } from '../../test/utils'
import type { Capabilities } from '../../api'

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

const projects = [
  { id: 'p1', org_id: 'o1', name: 'alpha', description: 'first project', created_at: '2026-01-01T00:00:00Z' },
]

const projectMembers = [
  { account_id: 'a@x', project_id: 'p1', role: 'viewer' },
  { account_id: 'b@x', project_id: 'p1', role: 'admin' },
]

function mockFetch() {
  vi.spyOn(globalThis, 'fetch').mockImplementation((input, init) => {
    const url = String(input).split('?')[0]
    const method = (init?.method ?? 'GET').toUpperCase()

    if (url.endsWith('/console/capabilities')) {
      return Promise.resolve(new Response(JSON.stringify(caps), { status: 200, headers: { 'content-type': 'application/json' } }))
    }
    if (url.endsWith('/console/projects') && method === 'GET') {
      return Promise.resolve(new Response(JSON.stringify({ org_id: 'o1', projects }), { status: 200, headers: { 'content-type': 'application/json' } }))
    }
    if (url.match(/\/console\/projects\/[^/]+\/members$/) && method === 'GET') {
      return Promise.resolve(new Response(JSON.stringify({ project_id: 'p1', members: projectMembers }), { status: 200, headers: { 'content-type': 'application/json' } }))
    }
    if (url.match(/\/console\/projects\/[^/]+\/members$/) && method === 'POST') {
      return Promise.resolve(new Response(JSON.stringify({}), { status: 201, headers: { 'content-type': 'application/json' } }))
    }
    if (url.match(/\/console\/projects\/[^/]+\/members\//) && method === 'DELETE') {
      return Promise.resolve(new Response('', { status: 204 }))
    }
    return Promise.resolve(new Response(JSON.stringify({}), { status: 200, headers: { 'content-type': 'application/json' } }))
  })
}

beforeEach(() => {
  mockFetch()
})

describe('ProjectDetail view accessibility', () => {
  it('has no axe violations with the members table and assign form rendered', async () => {
    const { container } = await renderAt('/projects/p1', caps)
    // Wait for the member table to load before auditing.
    await waitFor(() => expect(screen.getByText('a@x')).toBeInTheDocument())
    // Confirm labelled form controls are present.
    expect(screen.getByLabelText(/account id/i)).toBeInTheDocument()
    expect(screen.getByRole('combobox', { name: /role/i })).toBeInTheDocument()
    // Zero axe violations.
    expect(await axe(container)).toHaveNoViolations()
  })

  it('has no axe violations in the empty-members state', async () => {
    vi.spyOn(globalThis, 'fetch').mockImplementation((input, init) => {
      const url = String(input).split('?')[0]
      const method = (init?.method ?? 'GET').toUpperCase()
      if (url.endsWith('/console/capabilities')) {
        return Promise.resolve(new Response(JSON.stringify(caps), { status: 200, headers: { 'content-type': 'application/json' } }))
      }
      if (url.endsWith('/console/projects') && method === 'GET') {
        return Promise.resolve(new Response(JSON.stringify({ org_id: 'o1', projects }), { status: 200, headers: { 'content-type': 'application/json' } }))
      }
      if (url.match(/\/console\/projects\/[^/]+\/members$/) && method === 'GET') {
        return Promise.resolve(new Response(JSON.stringify({ project_id: 'p1', members: [] }), { status: 200, headers: { 'content-type': 'application/json' } }))
      }
      return Promise.resolve(new Response(JSON.stringify({}), { status: 200, headers: { 'content-type': 'application/json' } }))
    })
    const { container } = await renderAt('/projects/p1', caps)
    // Wait for the empty-state message to confirm rendering is complete.
    await waitFor(() => expect(screen.getByText(/no project members yet/i)).toBeInTheDocument())
    expect(await axe(container)).toHaveNoViolations()
  })
})
