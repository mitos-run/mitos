// Instance-operator audit view: asserts admin.* events (including a denied
// access attempt, action "admin.denied") render with actor/action/detail,
// and that an unauthenticated denial (empty actor_id) renders honestly
// rather than a blank cell.
import { describe, it, expect, vi } from 'vitest'
import { waitFor, screen } from '@testing-library/react'
import { renderAt } from '../../test/utils'
import type { Capabilities, AuditEvent } from '../../api'

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

function mockFetch(events: AuditEvent[]) {
  vi.spyOn(globalThis, 'fetch').mockImplementation((input) => {
    const url = String(input)
    if (url.endsWith('/console/capabilities')) {
      return Promise.resolve(new Response(JSON.stringify(caps), { status: 200, headers: { 'content-type': 'application/json' } }))
    }
    if (url.includes('/console/admin/audit')) {
      return Promise.resolve(new Response(JSON.stringify({ events }), { status: 200, headers: { 'content-type': 'application/json' } }))
    }
    return Promise.resolve(new Response(JSON.stringify({}), { status: 200, headers: { 'content-type': 'application/json' } }))
  })
}

describe('AdminAudit view', () => {
  it('renders admin.* events with actor, action, and detail', async () => {
    mockFetch([
      {
        org_id: '_instance',
        actor_id: 'acct-1',
        actor_name: 'Ops Alice',
        action: 'admin.overview.view',
        target: '',
        detail: 'viewed the instance operator overview',
        at: '2026-07-01T00:00:00Z',
      },
    ])
    await renderAt('/admin/audit', caps)
    await waitFor(() => expect(screen.getByText('Ops Alice')).toBeInTheDocument())
    expect(screen.getByText('admin.overview.view')).toBeInTheDocument()
    expect(screen.getByText('viewed the instance operator overview')).toBeInTheDocument()
  })

  it('shows an honest "unauthenticated" actor for a denial with no actor id', async () => {
    mockFetch([
      {
        org_id: '_instance',
        actor_id: '',
        action: 'admin.denied',
        target: '',
        target_type: 'system',
        detail: '/console/admin/overview',
        at: '2026-07-01T00:00:00Z',
      },
    ])
    await renderAt('/admin/audit', caps)
    await waitFor(() => expect(screen.getByText('admin.denied')).toBeInTheDocument())
    expect(screen.getByText('unauthenticated')).toBeInTheDocument()
    expect(screen.getByText('/console/admin/overview')).toBeInTheDocument()
  })

  it('shows an empty state when there are no operator events yet', async () => {
    mockFetch([])
    await renderAt('/admin/audit', caps)
    await waitFor(() => expect(screen.getByText(/no operator activity yet/i)).toBeInTheDocument())
  })
})
