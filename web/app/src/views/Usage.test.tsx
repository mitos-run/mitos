// Focused mobile-pass test for Usage: the totals/cost tiles use the shared
// .stat-grid class (base.css), whose overflow-safe minmax() collapses the
// grid to one column on a narrow viewport without ever forcing the page body
// to scroll horizontally.
import { describe, it, expect, vi } from 'vitest'
import { waitFor, screen } from '@testing-library/react'
import { renderAt } from '../test/utils'
import type { Capabilities, UsageResponse } from '../api'

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

const usagePayload: UsageResponse = {
  org_id: 'o1',
  records: [],
  totals: { sandboxes_created: 12, vcpu_seconds: 3600 },
  cost: { compute_cents: 500 },
}

function mockFetch() {
  vi.spyOn(globalThis, 'fetch').mockImplementation((input) => {
    const url = String(input)
    if (url.endsWith('/console/capabilities')) {
      return Promise.resolve(new Response(JSON.stringify(caps), { status: 200, headers: { 'content-type': 'application/json' } }))
    }
    if (url.includes('/console/usage')) {
      return Promise.resolve(new Response(JSON.stringify(usagePayload), { status: 200, headers: { 'content-type': 'application/json' } }))
    }
    return Promise.resolve(new Response(JSON.stringify({}), { status: 200, headers: { 'content-type': 'application/json' } }))
  })
}

describe('Usage view', () => {
  it('lays out totals and cost tiles in the shared overflow-safe stat-grid', async () => {
    mockFetch()
    const { container } = await renderAt('/usage', caps)
    await waitFor(() => expect(screen.getByText(/sandboxes created/i)).toBeInTheDocument())
    expect(container.querySelector('.stat-grid')).not.toBeNull()
  })
})
