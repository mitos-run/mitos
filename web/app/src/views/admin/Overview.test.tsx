// Instance-operator overview: asserts the failed_orgs muted note (a per-org
// read failure that the server skips rather than 500ing the whole overview,
// see internal/saas/console/admin.go's handleAdminOverview) renders only
// when failed_orgs is nonzero, reusing the same t-dim muted-text convention
// the rest of this view already uses for its "not available" states.
import { describe, it, expect, vi } from 'vitest'
import { waitFor, screen } from '@testing-library/react'
import { renderAt } from '../../test/utils'
import type { Capabilities, AdminOverview as AdminOverviewResponse } from '../../api'

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

function mockFetch(overview: AdminOverviewResponse) {
  vi.spyOn(globalThis, 'fetch').mockImplementation((input) => {
    const url = String(input)
    if (url.endsWith('/console/capabilities')) {
      return Promise.resolve(new Response(JSON.stringify(caps), { status: 200, headers: { 'content-type': 'application/json' } }))
    }
    if (url.includes('/console/admin/overview')) {
      return Promise.resolve(new Response(JSON.stringify(overview), { status: 200, headers: { 'content-type': 'application/json' } }))
    }
    return Promise.resolve(new Response(JSON.stringify({}), { status: 200, headers: { 'content-type': 'application/json' } }))
  })
}

describe('AdminOverview view', () => {
  it('shows a muted note when failed_orgs is nonzero', async () => {
    mockFetch({
      orgs: 3,
      running_sandboxes: 5,
      running_sandboxes_orgs: 3,
      nodes_ready: null,
      nodes_total: null,
      signup_mode: 'open',
      failed_orgs: 2,
    })
    await renderAt('/admin', caps)
    await waitFor(() => expect(screen.getByText(/could not be read/i)).toBeInTheDocument())
    expect(screen.getByText(/2 organizations could not be read/i)).toBeInTheDocument()
  })

  it('renders no failed-orgs note when failed_orgs is absent', async () => {
    mockFetch({ orgs: 3, running_sandboxes: 5, running_sandboxes_orgs: 3, nodes_ready: null, nodes_total: null, signup_mode: 'open' })
    await renderAt('/admin', caps)
    await waitFor(() => expect(screen.getByText('Organizations')).toBeInTheDocument())
    expect(screen.queryByText(/could not be read/i)).not.toBeInTheDocument()
  })

  it('renders no failed-orgs note when failed_orgs is zero', async () => {
    mockFetch({ orgs: 3, running_sandboxes: 5, running_sandboxes_orgs: 3, nodes_ready: null, nodes_total: null, signup_mode: 'open', failed_orgs: 0 })
    await renderAt('/admin', caps)
    await waitFor(() => expect(screen.getByText('Organizations')).toBeInTheDocument())
    expect(screen.queryByText(/could not be read/i)).not.toBeInTheDocument()
  })

  // The running-sandboxes tile must carry the same "showing first N of
  // orgs" honesty disclosure the orgs table already has once the 200-org
  // rollup cap is hit (issue #714): previously it silently implied full
  // coverage regardless of deployment size.
  it('shows a rollup-cap disclosure under the running-sandboxes tile when running_sandboxes_orgs is less than orgs', async () => {
    mockFetch({
      orgs: 250,
      running_sandboxes: 40,
      running_sandboxes_orgs: 200,
      nodes_ready: null,
      nodes_total: null,
      signup_mode: 'open',
    })
    await renderAt('/admin', caps)
    await waitFor(() => expect(screen.getByText('Running sandboxes')).toBeInTheDocument())
    expect(screen.getByText(/showing sandboxes from the first 200 of 250 organizations/i)).toBeInTheDocument()
  })

  it('omits the rollup-cap disclosure when every org was scanned', async () => {
    mockFetch({
      orgs: 3,
      running_sandboxes: 5,
      running_sandboxes_orgs: 3,
      nodes_ready: null,
      nodes_total: null,
      signup_mode: 'open',
    })
    await renderAt('/admin', caps)
    await waitFor(() => expect(screen.getByText('Running sandboxes')).toBeInTheDocument())
    expect(screen.queryByText(/showing sandboxes from the first/i)).not.toBeInTheDocument()
  })
})
