// TDD suite for ProjectDetail: per-project member management.
// Tests written before any implementation (RED phase).
import { describe, it, expect, vi, beforeEach } from 'vitest'
import { fireEvent, waitFor, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { renderAt } from '../../test/utils'
import type { Capabilities } from '../../api'

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

const projectMembers = [
  { account_id: 'a@x', project_id: 'p1', role: 'viewer' },
]

const projects = [
  { id: 'p1', org_id: 'o1', name: 'alpha', description: 'first project', created_at: '2026-01-01T00:00:00Z' },
]

function mockFetch(overrides: { assignStatus?: number } = {}) {
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
      const status = overrides.assignStatus ?? 201
      return Promise.resolve(new Response(JSON.stringify({}), { status, headers: { 'content-type': 'application/json' } }))
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

describe('ProjectDetail view', () => {
  it('renders the page header with the project name', async () => {
    await renderAt('/projects/p1', caps)
    await waitFor(() => expect(screen.getByRole('heading', { level: 1 })).toHaveTextContent(/alpha/i))
  })

  it('renders the existing member row with account_id and role', async () => {
    await renderAt('/projects/p1', caps)
    await waitFor(() => expect(screen.getByText('a@x')).toBeInTheDocument())
    // The role badge appears in the table row
    const badge = document.querySelector('.role-badge')
    expect(badge).toBeTruthy()
    expect(badge?.textContent).toBe('viewer')
  })

  it('renders a Revoke button for each member', async () => {
    await renderAt('/projects/p1', caps)
    await waitFor(() => expect(screen.getByText('a@x')).toBeInTheDocument())
    expect(screen.getByRole('button', { name: /revoke/i })).toBeInTheDocument()
  })

  it('renders the assign form with account input, role select, and Save button', async () => {
    await renderAt('/projects/p1', caps)
    await waitFor(() => expect(screen.getByRole('heading', { level: 1 })).toBeInTheDocument())
    expect(screen.getByLabelText(/account id/i)).toBeInTheDocument()
    expect(screen.getByRole('combobox', { name: /role/i })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: /assign|save/i })).toBeInTheDocument()
  })

  it('the role select contains all 5 built-in roles', async () => {
    await renderAt('/projects/p1', caps)
    await waitFor(() => expect(screen.getByRole('combobox', { name: /role/i })).toBeInTheDocument())
    const select = screen.getByRole('combobox', { name: /role/i })
    const options = Array.from(select.querySelectorAll('option')).map((o) => o.value)
    expect(options).toContain('owner')
    expect(options).toContain('admin')
    expect(options).toContain('billing')
    expect(options).toContain('member')
    expect(options).toContain('viewer')
  })

  it('calls POST /console/projects/p1/members when assign form is submitted', async () => {
    const postSpy = vi.fn().mockResolvedValue(new Response(JSON.stringify({}), { status: 201, headers: { 'content-type': 'application/json' } }))
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
        return postSpy(input, init)
      }
      return Promise.resolve(new Response(JSON.stringify({}), { status: 200, headers: { 'content-type': 'application/json' } }))
    })

    await renderAt('/projects/p1', caps)
    await waitFor(() => expect(screen.getByLabelText(/account id/i)).toBeInTheDocument())

    await userEvent.type(screen.getByLabelText(/account id/i), 'new@user')
    fireEvent.change(screen.getByRole('combobox', { name: /role/i }), { target: { value: 'admin' } })
    fireEvent.click(screen.getByRole('button', { name: /assign|save/i }))

    await waitFor(() => expect(postSpy).toHaveBeenCalled())
    const [calledUrl, calledInit] = postSpy.mock.calls[0] as [string, RequestInit]
    expect(calledUrl).toContain('/console/projects/p1/members')
    const body = JSON.parse(calledInit.body as string) as { account_id: string; role: string }
    expect(body.account_id).toBe('new@user')
    expect(body.role).toBe('admin')
  })

  it('shows the empty state note when there are no members', async () => {
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
    await renderAt('/projects/p1', caps)
    await waitFor(() => expect(screen.getByText(/no project members yet/i)).toBeInTheDocument())
  })

  it('shows an honest note about per-project roles', async () => {
    await renderAt('/projects/p1', caps)
    await waitFor(() => expect(screen.getByRole('heading', { level: 1 })).toBeInTheDocument())
    expect(screen.getByText(/per-project roles take effect/i)).toBeInTheDocument()
  })
})
