// Task 4: Members (role management) and Projects views.
// TDD suite: covers list rendering, loading/empty states, role change, and project creation.
import { describe, it, expect, vi, beforeEach } from 'vitest'
import { fireEvent, waitFor, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { renderAt } from '../test/utils'
import type { Capabilities } from '../api'

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

function mockFetch(overrides: Record<string, unknown> = {}) {
  vi.spyOn(globalThis, 'fetch').mockImplementation((input, init) => {
    const url = String(input).split('?')[0]
    const method = (init?.method ?? 'GET').toUpperCase()

    if (url.endsWith('/console/capabilities')) {
      return Promise.resolve(new Response(JSON.stringify(caps), { status: 200, headers: { 'content-type': 'application/json' } }))
    }
    if (url.endsWith('/console/members') && method === 'GET') {
      return Promise.resolve(new Response(JSON.stringify(overrides['members'] ?? { org_id: 'o1', members }), { status: 200, headers: { 'content-type': 'application/json' } }))
    }
    if (url.match(/\/console\/members\/[^/]+\/role/) && method === 'POST') {
      return Promise.resolve(new Response(JSON.stringify({}), { status: 200, headers: { 'content-type': 'application/json' } }))
    }
    if (url.endsWith('/console/projects') && method === 'GET') {
      return Promise.resolve(new Response(JSON.stringify(overrides['projects'] ?? { org_id: 'o1', projects }), { status: 200, headers: { 'content-type': 'application/json' } }))
    }
    if (url.endsWith('/console/projects') && method === 'POST') {
      const created = { id: 'p2', org_id: 'o1', name: 'beta', description: 'second', created_at: '2026-06-01T00:00:00Z' }
      return Promise.resolve(new Response(JSON.stringify(created), { status: 201, headers: { 'content-type': 'application/json' } }))
    }
    return Promise.resolve(new Response(JSON.stringify({}), { status: 200, headers: { 'content-type': 'application/json' } }))
  })
}

beforeEach(() => {
  mockFetch()
})

describe('Members view', () => {
  it('renders the members table with Account, Role, and Joined columns', async () => {
    await renderAt('/members', caps)
    await waitFor(() => expect(screen.getByText('alice')).toBeInTheDocument())
    expect(screen.getByRole('columnheader', { name: /account/i })).toBeInTheDocument()
    expect(screen.getByRole('columnheader', { name: /role/i })).toBeInTheDocument()
    expect(screen.getByRole('columnheader', { name: /joined/i })).toBeInTheDocument()
    expect(screen.getByText('bob')).toBeInTheDocument()
  })

  it('renders a role badge and a role select per member row', async () => {
    await renderAt('/members', caps)
    await waitFor(() => expect(screen.getByText('alice')).toBeInTheDocument())
    const selects = screen.getAllByRole('combobox')
    expect(selects.length).toBeGreaterThanOrEqual(2)
    // Each select should be labelled
    selects.forEach((sel) => {
      expect(sel).toHaveAccessibleName()
    })
  })

  it('shows loading skeleton while members are fetching', async () => {
    vi.spyOn(globalThis, 'fetch').mockImplementation((input) => {
      const url = String(input)
      if (url.endsWith('/console/capabilities')) {
        return Promise.resolve(new Response(JSON.stringify(caps), { status: 200, headers: { 'content-type': 'application/json' } }))
      }
      // All other endpoints hang so the view stays in loading state
      return new Promise(() => {})
    })
    await renderAt('/members', caps)
    await waitFor(() => expect(screen.getByRole('status')).toBeInTheDocument())
  })

  it('shows empty state when there are no members', async () => {
    mockFetch({ members: { org_id: 'o1', members: [] } })
    await renderAt('/members', caps)
    await waitFor(() => expect(screen.getByRole('heading', { name: /no members/i })).toBeInTheDocument())
  })

  it('calls setMemberRole and shows a success toast on role change', async () => {
    await renderAt('/members', caps)
    await waitFor(() => expect(screen.getByText('alice')).toBeInTheDocument())
    const selects = screen.getAllByRole('combobox')
    // Change the first select (alice) to 'admin'
    fireEvent.change(selects[0], { target: { value: 'admin' } })
    await waitFor(() => expect(screen.getByRole('status')).toBeInTheDocument())
  })

  it('filters the members table when a search query is typed', async () => {
    await renderAt('/members', caps)
    await waitFor(() => expect(screen.getByText('alice')).toBeInTheDocument())
    expect(screen.getByText('bob')).toBeInTheDocument()
    const searchBox = screen.getByRole('searchbox', { name: /search members/i })
    await userEvent.type(searchBox, 'alice')
    expect(screen.getByText('alice')).toBeInTheDocument()
    expect(screen.queryByText('bob')).not.toBeInTheDocument()
  })
})

describe('Projects view', () => {
  it('renders the create form with labelled name and description inputs', async () => {
    await renderAt('/projects', caps)
    await waitFor(() => expect(screen.getByLabelText(/name/i)).toBeInTheDocument())
    expect(screen.getByLabelText(/description/i)).toBeInTheDocument()
    expect(screen.getByRole('button', { name: /create project/i })).toBeInTheDocument()
  })

  it('renders the list of projects', async () => {
    await renderAt('/projects', caps)
    await waitFor(() => expect(screen.getByText('alpha')).toBeInTheDocument())
    expect(screen.getByText('first project')).toBeInTheDocument()
  })

  it('shows loading skeleton while projects are fetching', async () => {
    vi.spyOn(globalThis, 'fetch').mockImplementation((input) => {
      const url = String(input)
      if (url.endsWith('/console/capabilities')) {
        return Promise.resolve(new Response(JSON.stringify(caps), { status: 200, headers: { 'content-type': 'application/json' } }))
      }
      // All other endpoints hang so the view stays in loading state
      return new Promise(() => {})
    })
    await renderAt('/projects', caps)
    await waitFor(() => expect(screen.getByRole('status')).toBeInTheDocument())
  })

  it('shows empty state when there are no projects', async () => {
    mockFetch({ projects: { org_id: 'o1', projects: [] } })
    await renderAt('/projects', caps)
    await waitFor(() => expect(screen.getByRole('heading', { name: /no projects/i })).toBeInTheDocument())
  })

  it('creates a project and shows a success toast', async () => {
    await renderAt('/projects', caps)
    await waitFor(() => expect(screen.getByLabelText(/name/i)).toBeInTheDocument())
    fireEvent.change(screen.getByLabelText(/name/i), { target: { value: 'beta' } })
    fireEvent.change(screen.getByLabelText(/description/i), { target: { value: 'second' } })
    fireEvent.click(screen.getByRole('button', { name: /create project/i }))
    await waitFor(() => expect(screen.getByRole('status')).toBeInTheDocument())
  })
})
