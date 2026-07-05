// Instance-operator org table: asserts the failed_orgs muted note (a per-org
// read failure the server skips rather than 500ing the whole table, see
// internal/saas/console/admin.go's handleAdminOrgs) renders only when
// failed_orgs is nonzero, reusing the same t-dim muted-text convention this
// view already uses for its "showing N of M" capped-rollup note.
import { describe, it, expect, vi } from 'vitest'
import { waitFor, screen } from '@testing-library/react'
import { renderAt } from '../../test/utils'
import type { Capabilities, AdminOrgsResponse } from '../../api'

const caps: Capabilities = {
  edition: 'hosted',
  billing: true,
  signup: true,
  teams: true,
  idp: 'oidc',
  orgSwitcher: false,
  secrets: { providers: [] },
  proof: true,
  ownership: 'hosted',
  admin: true,
}

const orgRow = { id: 'o1', name: 'Acme', tier: 'free', members: 2, running: 1, month_usage_cents: 500 }

function mockFetch(orgs: AdminOrgsResponse) {
  vi.spyOn(globalThis, 'fetch').mockImplementation((input) => {
    const url = String(input)
    if (url.endsWith('/console/capabilities')) {
      return Promise.resolve(new Response(JSON.stringify(caps), { status: 200, headers: { 'content-type': 'application/json' } }))
    }
    if (url.includes('/console/admin/orgs')) {
      return Promise.resolve(new Response(JSON.stringify(orgs), { status: 200, headers: { 'content-type': 'application/json' } }))
    }
    return Promise.resolve(new Response(JSON.stringify({}), { status: 200, headers: { 'content-type': 'application/json' } }))
  })
}

describe('AdminOrgs view', () => {
  it('shows a muted note when failed_orgs is nonzero', async () => {
    mockFetch({ orgs: [orgRow], total: 1, failed_orgs: 1 })
    await renderAt('/admin/orgs', caps)
    await waitFor(() => expect(screen.getByText(/could not be read/i)).toBeInTheDocument())
    expect(screen.getByText(/1 organization could not be read/i)).toBeInTheDocument()
  })

  it('renders no failed-orgs note when failed_orgs is absent', async () => {
    mockFetch({ orgs: [orgRow], total: 1 })
    await renderAt('/admin/orgs', caps)
    await waitFor(() => expect(screen.getByText('Acme')).toBeInTheDocument())
    expect(screen.queryByText(/could not be read/i)).not.toBeInTheDocument()
  })

  it('renders no failed-orgs note when failed_orgs is zero', async () => {
    mockFetch({ orgs: [orgRow], total: 1, failed_orgs: 0 })
    await renderAt('/admin/orgs', caps)
    await waitFor(() => expect(screen.getByText('Acme')).toBeInTheDocument())
    expect(screen.queryByText(/could not be read/i)).not.toBeInTheDocument()
  })
})
