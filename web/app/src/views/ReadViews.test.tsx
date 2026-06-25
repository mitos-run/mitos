import { describe, it, expect, vi, beforeEach } from 'vitest'
import { waitFor, screen } from '@testing-library/react'
import { renderAt } from '../test/utils'
import type { Capabilities } from '../api'

const caps: Capabilities = { edition: 'community', billing: true, signup: false, teams: true, idp: 'oidc', orgSwitcher: false, secrets: { providers: ['kube'] }, proof: true, ownership: 'hosted' }

function mockFor(map: Record<string, unknown>) {
  vi.spyOn(globalThis, 'fetch').mockImplementation((input) => {
    const url = String(input).split('?')[0]
    const key = Object.keys(map).find((k) => url.endsWith(k))
    return Promise.resolve(new Response(JSON.stringify(key ? map[key] : {}), { status: 200, headers: { 'content-type': 'application/json' } }))
  })
}

beforeEach(() => {
  mockFor({
    '/console/capabilities': caps,
    '/console/audit': { org_id: 'o', events: [{ org_id: 'o', actor_id: 'alice', action: 'key.create', target: 'k1', detail: 'created api key mitos_live_ab12', at: '2026-06-25T08:00:00Z' }] },
    '/console/templates': { org_id: 'o', templates: [{ name: 'python-3.12', org_id: 'o', description: 'Python 3.12', image: 'ghcr.io/x/py:3.12', updated_at: '2026-06-01T00:00:00Z' }] },
    '/console/billing': { org_id: 'o', status: 'active', balance_cents: 5000, spend_cents: 1234, soft_cap_cents: 0, hard_cap_cents: 0, ledger_entries: [] },
    '/console/usage': { org_id: 'o', records: [], totals: { vcpu_seconds: 3600 }, cost: { total_cents: 1234 } },
  })
})

describe('read views', () => {
  it('audit shows an event', async () => {
    await renderAt('/audit', caps)
    await waitFor(() => expect(screen.getByText('key.create')).toBeInTheDocument())
    expect(screen.getByText('alice')).toBeInTheDocument()
  })
  it('templates shows a template', async () => {
    await renderAt('/templates', caps)
    await waitFor(() => expect(screen.getByText('python-3.12')).toBeInTheDocument())
  })
  it('billing shows status when capability is on', async () => {
    await renderAt('/billing', caps)
    await waitFor(() => expect(screen.getByText(/active/i)).toBeInTheDocument())
  })
})
