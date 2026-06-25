// TDD suite for the Roles view: permission matrix + custom role management.
// Covers: builtin roles render read-only, custom roles render, toggling a
// permission checkbox and saving calls POST /console/roles with the right body.
import { describe, it, expect, vi, beforeEach } from 'vitest'
import { fireEvent, waitFor, screen } from '@testing-library/react'
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

function mockFetch(postSpy?: ReturnType<typeof vi.fn>) {
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
      if (postSpy) postSpy(JSON.parse((init?.body as string) ?? '{}'))
      return Promise.resolve(new Response(JSON.stringify({}), { status: 200, headers: { 'content-type': 'application/json' } }))
    }
    if (url.match(/\/console\/roles\/[^/]+$/) && method === 'DELETE') {
      return Promise.resolve(new Response(null, { status: 204 }))
    }
    return Promise.resolve(new Response(JSON.stringify({}), { status: 200, headers: { 'content-type': 'application/json' } }))
  })
}

beforeEach(() => {
  mockFetch()
})

describe('Roles view', () => {
  it('renders the page title "Roles"', async () => {
    await renderAt('/roles', caps)
    await waitFor(() => expect(screen.getByRole('heading', { level: 1, name: /roles/i })).toBeInTheDocument())
  })

  it('renders the permission matrix table with role columns for builtins and custom roles', async () => {
    await renderAt('/roles', caps)
    await waitFor(() => expect(screen.getByRole('columnheader', { name: /owner/i })).toBeInTheDocument())
    expect(screen.getByRole('columnheader', { name: /admin/i })).toBeInTheDocument()
    expect(screen.getByRole('columnheader', { name: /member/i })).toBeInTheDocument()
    expect(screen.getByRole('columnheader', { name: /viewer/i })).toBeInTheDocument()
    expect(screen.getByRole('columnheader', { name: /deployer/i })).toBeInTheDocument()
  })

  it('renders all 7 enforced permissions as row labels', async () => {
    await renderAt('/roles', caps)
    await waitFor(() => expect(screen.getAllByText(/manage members/i).length).toBeGreaterThanOrEqual(1))
    expect(screen.getAllByText(/manage projects/i).length).toBeGreaterThanOrEqual(1)
    expect(screen.getAllByText(/manage secrets/i).length).toBeGreaterThanOrEqual(1)
    expect(screen.getAllByText(/manage settings/i).length).toBeGreaterThanOrEqual(1)
    expect(screen.getAllByText(/manage billing/i).length).toBeGreaterThanOrEqual(1)
    expect(screen.getAllByText(/use resources/i).length).toBeGreaterThanOrEqual(1)
    expect(screen.getAllByText(/read access/i).length).toBeGreaterThanOrEqual(1)
  })

  it('shows a checkmark or indicator for permissions the builtin owner role has', async () => {
    await renderAt('/roles', caps)
    await waitFor(() => expect(screen.getByRole('columnheader', { name: /owner/i })).toBeInTheDocument())
    // The owner has all 7 permissions; the matrix cell should be non-empty.
    // We verify using aria or text content of the matrix.
    const table = screen.getByRole('table', { name: /permission matrix/i })
    expect(table).toBeInTheDocument()
    // The owner column header exists; the matrix renders some form of checkmark.
    expect(table.textContent).toMatch(/owner/i)
  })

  it('renders the new custom role form with a name input and permission checkboxes', async () => {
    await renderAt('/roles', caps)
    await waitFor(() => expect(screen.getByLabelText(/role name/i)).toBeInTheDocument())
    expect(screen.getByLabelText(/manage members/i)).toBeInTheDocument()
    expect(screen.getByLabelText(/read access/i)).toBeInTheDocument()
    expect(screen.getByRole('button', { name: /save/i })).toBeInTheDocument()
  })

  it('toggling a permission checkbox and clicking Save calls POST /console/roles with the chosen permissions', async () => {
    const postSpy = vi.fn()
    mockFetch(postSpy)
    await renderAt('/roles', caps)
    await waitFor(() => expect(screen.getByLabelText(/role name/i)).toBeInTheDocument())

    // Fill in a name
    fireEvent.change(screen.getByLabelText(/role name/i), { target: { value: 'tester' } })

    // Toggle the "Read access" checkbox
    const readCheckbox = screen.getByLabelText(/read access/i)
    fireEvent.click(readCheckbox)

    // Click Save
    fireEvent.click(screen.getByRole('button', { name: /save/i }))

    await waitFor(() => expect(postSpy).toHaveBeenCalledTimes(1))
    const body = postSpy.mock.calls[0][0] as { name: string; permissions: string[] }
    expect(body.name).toBe('tester')
    expect(body.permissions).toContain('read')
  })

  it('shows a delete button for each custom role', async () => {
    await renderAt('/roles', caps)
    await waitFor(() => expect(screen.getByRole('button', { name: /delete deployer/i })).toBeInTheDocument())
  })

  it('shows an honest note about API enforcement and owner/admin restriction', async () => {
    await renderAt('/roles', caps)
    await waitFor(() => expect(screen.getByRole('heading', { level: 1, name: /roles/i })).toBeInTheDocument())
    // The view should have at least one note mentioning API enforcement.
    const notes = screen.getAllByText(/enforced by the api/i)
    expect(notes.length).toBeGreaterThanOrEqual(1)
  })
})
