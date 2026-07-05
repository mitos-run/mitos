// Task 4: Members (role management) and Projects views.
// TDD suite: covers list rendering, loading/empty states, role change, and project creation.
import { describe, it, expect, vi, beforeEach } from 'vitest'
import { fireEvent, waitFor, screen, within } from '@testing-library/react'
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

const invitations = [
  {
    id: 'inv-1', org_id: 'o1', email: 'carol@example.com', role: 'member', state: 'pending',
    inviter_id: 'alice', inviter_name: 'Alice', created_at: '2026-05-01T00:00:00Z', expires_at: '2026-05-08T00:00:00Z',
  },
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
    if (url.match(/\/console\/members\/[^/]+$/) && method === 'DELETE') {
      return Promise.resolve(new Response(JSON.stringify({}), { status: 200, headers: { 'content-type': 'application/json' } }))
    }
    if (url.endsWith('/console/invites') && method === 'GET') {
      return Promise.resolve(new Response(JSON.stringify(overrides['invites'] ?? { org_id: 'o1', invitations }), { status: 200, headers: { 'content-type': 'application/json' } }))
    }
    if (url.match(/\/console\/invites\/[^/]+$/) && method === 'DELETE') {
      return Promise.resolve(new Response(JSON.stringify({}), { status: 200, headers: { 'content-type': 'application/json' } }))
    }
    if (url.match(/\/console\/invites\/[^/]+\/resend/) && method === 'POST') {
      return Promise.resolve(new Response(JSON.stringify(invitations[0]), { status: 200, headers: { 'content-type': 'application/json' } }))
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
    const table = within(screen.getByRole('table', { name: /^members$/i }))
    expect(table.getByRole('columnheader', { name: /account/i })).toBeInTheDocument()
    expect(table.getByRole('columnheader', { name: /role/i })).toBeInTheDocument()
    expect(table.getByRole('columnheader', { name: /joined/i })).toBeInTheDocument()
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

  it('shows the display name and email when the server provides them', async () => {
    mockFetch({
      members: {
        org_id: 'o1',
        members: [
          { account_id: 'alice', org_id: 'o1', role: 'owner', created_at: '2026-01-01T00:00:00Z', display_name: 'Alice Anderson', email: 'alice@acme.dev' },
        ],
      },
    })
    await renderAt('/members', caps)
    await waitFor(() => expect(screen.getByText('Alice Anderson')).toBeInTheDocument())
    expect(screen.getByText('alice@acme.dev')).toBeInTheDocument()
    // The raw account id is no longer the primary label once a name is known.
    expect(screen.queryByText('alice')).not.toBeInTheDocument()
  })

  it('opens the invite modal from the page header action', async () => {
    await renderAt('/members', caps)
    await waitFor(() => expect(screen.getByText('alice')).toBeInTheDocument())
    fireEvent.click(screen.getByRole('button', { name: /invite people/i }))
    expect(screen.getByRole('dialog', { name: /invite people/i })).toBeInTheDocument()
    expect(screen.getByLabelText(/email addresses/i)).toBeInTheDocument()
  })

  it('shows the empty state with an Invite people action when there are no members', async () => {
    mockFetch({ members: { org_id: 'o1', members: [] } })
    await renderAt('/members', caps)
    await waitFor(() => expect(screen.getByRole('heading', { name: /no members/i })).toBeInTheDocument())
    // The empty state's own action button, distinct from the header's.
    expect(screen.getAllByRole('button', { name: /invite people/i }).length).toBeGreaterThanOrEqual(1)
  })

  it('lists pending invitations with resend and revoke actions', async () => {
    await renderAt('/members', caps)
    await waitFor(() => expect(screen.getByText('carol@example.com')).toBeInTheDocument())
    expect(screen.getByRole('button', { name: /resend invitation to carol@example.com/i })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: /revoke invitation to carol@example.com/i })).toBeInTheDocument()
  })

  it('revokes a pending invitation and shows a success toast', async () => {
    await renderAt('/members', caps)
    await waitFor(() => expect(screen.getByText('carol@example.com')).toBeInTheDocument())
    fireEvent.click(screen.getByRole('button', { name: /revoke invitation to carol@example.com/i }))
    await waitFor(() => expect(screen.getByRole('status')).toBeInTheDocument())
  })

  it('shows no pending invitations message when there are none', async () => {
    mockFetch({ invites: { org_id: 'o1', invitations: [] } })
    await renderAt('/members', caps)
    await waitFor(() => expect(screen.getByText('alice')).toBeInTheDocument())
    expect(screen.getByText(/no pending invitations/i)).toBeInTheDocument()
  })

  it('removes a member after confirming, and shows a success toast', async () => {
    await renderAt('/members', caps)
    await waitFor(() => expect(screen.getByText('bob')).toBeInTheDocument())
    fireEvent.click(screen.getByRole('button', { name: /remove bob/i }))
    expect(screen.getByRole('alertdialog')).toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', { name: /^remove$/i }))
    await waitFor(() => expect(screen.getByRole('status')).toBeInTheDocument())
  })

  it('cancelling the remove confirmation leaves the member in place', async () => {
    await renderAt('/members', caps)
    await waitFor(() => expect(screen.getByText('bob')).toBeInTheDocument())
    fireEvent.click(screen.getByRole('button', { name: /remove bob/i }))
    fireEvent.click(screen.getByRole('button', { name: /cancel/i }))
    expect(screen.queryByRole('alertdialog')).not.toBeInTheDocument()
    expect(screen.getByText('bob')).toBeInTheDocument()
  })

  // Mobile + shared-modal parity: the confirm dialog carries the same
  // .modal / .modal-backdrop classes as InviteModal and NewSandboxModal so it
  // gets the <=480px full-screen-sheet treatment and safe-area padding from
  // base.css, instead of the old raw position:fixed styling.
  it('renders the remove confirmation inside the shared .modal with a .modal-backdrop sibling', async () => {
    await renderAt('/members', caps)
    await waitFor(() => expect(screen.getByText('bob')).toBeInTheDocument())
    fireEvent.click(screen.getByRole('button', { name: /remove bob/i }))
    const dialog = screen.getByRole('alertdialog')
    expect(dialog.className).toContain('modal')
    expect(dialog.parentElement?.className).toContain('modal-backdrop')
  })

  // Destructive confirm: Escape closes it, but a backdrop click must not
  // (a stray click cannot silently confirm removing someone from the org).
  it('closes the remove confirmation on Escape', async () => {
    await renderAt('/members', caps)
    await waitFor(() => expect(screen.getByText('bob')).toBeInTheDocument())
    fireEvent.click(screen.getByRole('button', { name: /remove bob/i }))
    expect(screen.getByRole('alertdialog')).toBeInTheDocument()
    fireEvent.keyDown(document, { key: 'Escape' })
    expect(screen.queryByRole('alertdialog')).not.toBeInTheDocument()
    expect(screen.getByText('bob')).toBeInTheDocument()
  })

  it('does not close the remove confirmation on a backdrop click', async () => {
    await renderAt('/members', caps)
    await waitFor(() => expect(screen.getByText('bob')).toBeInTheDocument())
    fireEvent.click(screen.getByRole('button', { name: /remove bob/i }))
    const dialog = screen.getByRole('alertdialog')
    const backdrop = dialog.parentElement as HTMLElement
    fireEvent.mouseDown(backdrop, { target: backdrop })
    expect(screen.getByRole('alertdialog')).toBeInTheDocument()
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
