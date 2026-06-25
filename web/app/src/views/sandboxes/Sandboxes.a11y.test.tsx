import { describe, it, expect, vi, beforeEach } from 'vitest'
import { waitFor, screen } from '@testing-library/react'
import { axe } from 'vitest-axe'
import * as matchers from 'vitest-axe/matchers'
import { renderAt } from '../../test/utils'
import type { Capabilities } from '../../api'

expect.extend(matchers)
const caps: Capabilities = {
  edition: 'community', billing: false, signup: false, teams: true, idp: 'oidc',
  orgSwitcher: false, secrets: { providers: ['kube'] }, proof: true, ownership: 'self-hosted',
}

const projects = [
  { id: 'proj-a', org_id: 'o', name: 'Alpha', description: '', created_at: '2026-01-01T00:00:00Z' },
]

beforeEach(() => {
  vi.spyOn(globalThis, 'fetch').mockImplementation((input) => {
    const url = String(input)
    if (url.endsWith('/console/capabilities')) return Promise.resolve(new Response(JSON.stringify(caps), { status: 200, headers: { 'content-type': 'application/json' } }))
    if (url.endsWith('/console/sandboxes')) return Promise.resolve(new Response(JSON.stringify({ org_id: 'o', sandboxes: [{ id: 's1', org_id: 'o', template: 't', node: 'n', phase: 'Running', vcpus: 1, mem_bytes: 1024, created_at: '', project_id: 'proj-a' }] }), { status: 200, headers: { 'content-type': 'application/json' } }))
    if (url.includes('/console/sandboxes/s1/logs')) return Promise.resolve(new Response('boot ok', { status: 200 }))
    if (url.includes('/console/sandboxes/s1')) return Promise.resolve(new Response(JSON.stringify({ id: 's1', org_id: 'o', template: 't', node: 'n', phase: 'Running', vcpus: 1, mem_bytes: 1024, created_at: '', project_id: 'proj-a' }), { status: 200, headers: { 'content-type': 'application/json' } }))
    if (url.endsWith('/console/projects')) return Promise.resolve(new Response(JSON.stringify({ org_id: 'o', projects }), { status: 200, headers: { 'content-type': 'application/json' } }))
    if (url.endsWith('/console/forktree')) return Promise.resolve(new Response(JSON.stringify({ org_id: 'o', nodes: [] }), { status: 200, headers: { 'content-type': 'application/json' } }))
    return Promise.resolve(new Response(JSON.stringify({}), { status: 200, headers: { 'content-type': 'application/json' } }))
  })
})

describe('Sandboxes accessibility', () => {
  it('the list has no axe violations', async () => {
    const { container } = await renderAt('/sandboxes', caps)
    await waitFor(() => expect(screen.getByRole('table', { name: /live sandboxes/i })).toBeInTheDocument())
    expect(await axe(container)).toHaveNoViolations()
  })

  it('the detail view with project control has no axe violations', async () => {
    const { container } = await renderAt('/sandboxes/s1', caps)
    // Wait until the project select is rendered and populated with the org projects.
    await waitFor(() => {
      const select = screen.getByLabelText(/project/i) as HTMLSelectElement
      expect(select).toBeInTheDocument()
      expect(screen.getByRole('option', { name: 'Alpha' })).toBeInTheDocument()
    })
    expect(await axe(container)).toHaveNoViolations()
  })
})
