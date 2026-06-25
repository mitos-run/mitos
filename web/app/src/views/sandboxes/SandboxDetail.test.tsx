import { describe, it, expect, vi, beforeEach } from 'vitest'
import { fireEvent, waitFor, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { renderAt } from '../../test/utils'
import type { Capabilities } from '../../api'

const caps: Capabilities = {
  edition: 'community', billing: false, signup: false, teams: true, idp: 'oidc',
  orgSwitcher: false, secrets: { providers: ['kube'] }, proof: true, ownership: 'self-hosted',
}

const projects = [
  { id: 'proj-a', org_id: 'o', name: 'Alpha', description: '', created_at: '2026-01-01T00:00:00Z' },
  { id: 'proj-b', org_id: 'o', name: 'Beta', description: '', created_at: '2026-01-02T00:00:00Z' },
]

let putCalls: { url: string; body: unknown }[] = []

beforeEach(() => {
  putCalls = []
  vi.spyOn(globalThis, 'fetch').mockImplementation((input, init) => {
    const url = String(input)
    const method = (init?.method ?? 'GET').toUpperCase()
    if (url.endsWith('/console/capabilities')) return Promise.resolve(new Response(JSON.stringify(caps), { status: 200, headers: { 'content-type': 'application/json' } }))
    if (url.includes('/console/sandboxes/s1/logs')) return Promise.resolve(new Response('boot ok\nlistening', { status: 200 }))
    if (url.includes('/console/sandboxes/s1/project') && method === 'PUT') {
      const body = init?.body ? JSON.parse(String(init.body)) as unknown : undefined
      putCalls.push({ url, body })
      return Promise.resolve(new Response(JSON.stringify({}), { status: 200, headers: { 'content-type': 'application/json' } }))
    }
    if (url.includes('/console/sandboxes/s1')) return Promise.resolve(new Response(JSON.stringify({ id: 's1', org_id: 'o', template: 'python-3.12', node: 'w1', phase: 'Running', vcpus: 2, mem_bytes: 2147483648, created_at: '2026-01-01T00:00:00Z', project_id: 'proj-a' }), { status: 200, headers: { 'content-type': 'application/json' } }))
    if (url.endsWith('/console/projects')) return Promise.resolve(new Response(JSON.stringify({ org_id: 'o', projects }), { status: 200, headers: { 'content-type': 'application/json' } }))
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

  it('renders a Project select with the current project pre-selected', async () => {
    await renderAt('/sandboxes/s1', caps)
    // wait until projects have loaded and the correct option is selected
    await waitFor(() => {
      const select = screen.getByLabelText(/project/i) as HTMLSelectElement
      expect(select.value).toBe('proj-a')
    })
  })

  it('includes an Unassigned option and options for each org project', async () => {
    await renderAt('/sandboxes/s1', caps)
    await waitFor(() => expect(screen.getByRole('option', { name: 'Alpha' })).toBeInTheDocument())
    expect(screen.getByRole('option', { name: /unassigned/i })).toBeInTheDocument()
    expect(screen.getByRole('option', { name: 'Beta' })).toBeInTheDocument()
  })

  it('calls PUT /console/sandboxes/{id}/project with the chosen project id on change', async () => {
    await renderAt('/sandboxes/s1', caps)
    await waitFor(() => expect(screen.getByRole('option', { name: 'Alpha' })).toBeInTheDocument())
    const select = screen.getByLabelText(/project/i)
    fireEvent.change(select, { target: { value: 'proj-b' } })
    await waitFor(() => expect(putCalls.length).toBe(1))
    expect(putCalls[0].url).toContain('/console/sandboxes/s1/project')
    expect(putCalls[0].body).toEqual({ project_id: 'proj-b' })
  })

  it('calls PUT with empty string when Unassigned is selected', async () => {
    await renderAt('/sandboxes/s1', caps)
    await waitFor(() => expect(screen.getByRole('option', { name: 'Alpha' })).toBeInTheDocument())
    const select = screen.getByLabelText(/project/i)
    fireEvent.change(select, { target: { value: '' } })
    await waitFor(() => expect(putCalls.length).toBe(1))
    expect(putCalls[0].body).toEqual({ project_id: '' })
  })
})
