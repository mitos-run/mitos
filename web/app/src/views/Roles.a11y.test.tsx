// Axe accessibility audit for the Roles view: no violations with the permission
// matrix table (real table with labelled toggles) and the new-role form
// (permission checkboxes with for/id label pairing).
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

const rolesPayload = {
  org_id: 'o1',
  builtins: [
    { name: 'owner', permissions: ['members.manage', 'projects.manage', 'secrets.manage', 'settings.manage', 'billing.manage', 'resources.use', 'read'] },
    { name: 'admin', permissions: ['projects.manage', 'secrets.manage', 'settings.manage', 'resources.use', 'read'] },
    { name: 'member', permissions: ['resources.use', 'read'] },
    { name: 'viewer', permissions: ['read'] },
  ],
  custom: [
    { name: 'deployer', permissions: ['resources.use', 'read'] },
  ],
}

beforeEach(() => {
  vi.spyOn(globalThis, 'fetch').mockImplementation((input, init) => {
    const url = String(input).split('?')[0]
    const method = (init?.method ?? 'GET').toUpperCase()
    if (url.endsWith('/console/capabilities')) {
      return Promise.resolve(new Response(JSON.stringify(caps), { status: 200, headers: { 'content-type': 'application/json' } }))
    }
    if (url.endsWith('/console/roles') && method === 'GET') {
      return Promise.resolve(new Response(JSON.stringify(rolesPayload), { status: 200, headers: { 'content-type': 'application/json' } }))
    }
    if (url.endsWith('/console/roles') && method === 'POST') {
      return Promise.resolve(new Response(JSON.stringify({}), { status: 200, headers: { 'content-type': 'application/json' } }))
    }
    if (url.match(/\/console\/roles\/[^/]+$/) && method === 'DELETE') {
      return Promise.resolve(new Response(null, { status: 204 }))
    }
    return Promise.resolve(new Response(JSON.stringify({}), { status: 200, headers: { 'content-type': 'application/json' } }))
  })
})

describe('Roles view accessibility', () => {
  it('has no axe violations with the permission matrix and new-role form rendered', async () => {
    const { container } = await renderAt('/roles', caps)
    // Wait for the matrix table to load: the owner column header is a reliable sentinel.
    await waitFor(() => expect(screen.getByRole('columnheader', { name: /owner/i })).toBeInTheDocument())
    // Confirm the permission checkboxes for the new-role form are labelled.
    expect(screen.getByLabelText(/manage members/i)).toBeInTheDocument()
    expect(screen.getByLabelText(/read access/i)).toBeInTheDocument()
    // Zero axe violations.
    expect(await axe(container)).toHaveNoViolations()
  })

  it('has no axe violations on the empty-custom-roles state (no custom roles section)', async () => {
    vi.spyOn(globalThis, 'fetch').mockImplementation((input, init) => {
      const url = String(input).split('?')[0]
      const method = (init?.method ?? 'GET').toUpperCase()
      if (url.endsWith('/console/capabilities')) {
        return Promise.resolve(new Response(JSON.stringify(caps), { status: 200, headers: { 'content-type': 'application/json' } }))
      }
      if (url.endsWith('/console/roles') && method === 'GET') {
        return Promise.resolve(
          new Response(
            JSON.stringify({ org_id: 'o1', builtins: rolesPayload.builtins, custom: [] }),
            { status: 200, headers: { 'content-type': 'application/json' } },
          ),
        )
      }
      return Promise.resolve(new Response(JSON.stringify({}), { status: 200, headers: { 'content-type': 'application/json' } }))
    })
    const { container } = await renderAt('/roles', caps)
    await waitFor(() => expect(screen.getByRole('columnheader', { name: /owner/i })).toBeInTheDocument())
    expect(await axe(container)).toHaveNoViolations()
  })
})
