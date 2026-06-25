import { describe, it, expect, vi, beforeEach } from 'vitest'
import { waitFor, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { renderAt } from '../../test/utils'
import type { Capabilities } from '../../api'

const caps: Capabilities = {
  edition: 'community', billing: false, signup: false, teams: true, idp: 'oidc',
  orgSwitcher: false, secrets: { providers: ['kube'] }, proof: true, ownership: 'self-hosted',
}

beforeEach(() => {
  vi.spyOn(globalThis, 'fetch').mockImplementation((input) => {
    const url = String(input)
    if (url.endsWith('/console/capabilities')) return Promise.resolve(new Response(JSON.stringify(caps), { status: 200, headers: { 'content-type': 'application/json' } }))
    if (url.includes('/console/sandboxes/s1/logs')) return Promise.resolve(new Response('boot ok\nlistening', { status: 200 }))
    if (url.includes('/console/sandboxes/s1')) return Promise.resolve(new Response(JSON.stringify({ id: 's1', org_id: 'o', template: 'python-3.12', node: 'w1', phase: 'Running', vcpus: 2, mem_bytes: 2147483648, created_at: '2026-01-01T00:00:00Z' }), { status: 200, headers: { 'content-type': 'application/json' } }))
    if (url.endsWith('/console/forktree')) return Promise.resolve(new Response(JSON.stringify({ org_id: 'o', nodes: [] }), { status: 200, headers: { 'content-type': 'application/json' } }))
    return Promise.resolve(new Response(JSON.stringify({}), { status: 200, headers: { 'content-type': 'application/json' } }))
  })
})

describe('SandboxDetail', () => {
  it('renders the sandbox overview and switches to the Logs tab', async () => {
    await renderAt('/sandboxes/s1', caps)
    await waitFor(() => expect(screen.getByRole('heading', { name: /s1/ })).toBeInTheDocument())
    expect(screen.getByText('python-3.12')).toBeInTheDocument()
    await userEvent.click(screen.getByRole('tab', { name: /logs/i }))
    await waitFor(() => expect(screen.getByText(/listening/)).toBeInTheDocument())
  })
})
