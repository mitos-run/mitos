// Audit view tests: filterable event table + retention panel + sinks panel.
import { describe, it, expect, vi, beforeEach } from 'vitest'
import { waitFor, screen } from '@testing-library/react'
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

beforeEach(() => {
  vi.spyOn(globalThis, 'fetch').mockImplementation((input) => {
    const url = String(input).split('?')[0]
    if (url.endsWith('/console/capabilities'))
      return Promise.resolve(new Response(JSON.stringify(caps), { status: 200, headers: { 'content-type': 'application/json' } }))
    if (url.endsWith('/console/audit'))
      return Promise.resolve(
        new Response(
          JSON.stringify({
            org_id: 'o',
            events: [{ org_id: 'o', actor_id: 'alice', action: 'key.create', target: 'k1', detail: 'created api key', at: '2026-06-25T08:00:00Z' }],
          }),
          { status: 200, headers: { 'content-type': 'application/json' } },
        ),
      )
    if (url.endsWith('/console/audit/retention'))
      return Promise.resolve(new Response(JSON.stringify({ days: 30 }), { status: 200, headers: { 'content-type': 'application/json' } }))
    if (url.endsWith('/console/audit/sinks'))
      return Promise.resolve(
        new Response(
          JSON.stringify({
            sinks: [{ id: 's1', org_id: 'o', type: 'webhook', endpoint: 'https://siem.example/hook', enabled: true, created_at: '2026-06-01T00:00:00Z' }],
          }),
          { status: 200, headers: { 'content-type': 'application/json' } },
        ),
      )
    return Promise.resolve(new Response(JSON.stringify({}), { status: 200, headers: { 'content-type': 'application/json' } }))
  })
})

describe('Audit view', () => {
  it('shows audit events from the event table', async () => {
    await renderAt('/audit', caps)
    await waitFor(() => expect(screen.getByText('key.create')).toBeInTheDocument())
    expect(screen.getByText('alice')).toBeInTheDocument()
  })

  it('shows the retention input with current value', async () => {
    await renderAt('/audit', caps)
    const retentionInput = await waitFor(() => screen.getByRole('spinbutton', { name: /retention/i }))
    expect(retentionInput).toHaveValue(30)
  })

  it('renders a sink row with the endpoint', async () => {
    await renderAt('/audit', caps)
    await waitFor(() => expect(screen.getByText('https://siem.example/hook')).toBeInTheDocument())
    // The sink table row contains the endpoint
    const endpointCell = screen.getByText('https://siem.example/hook')
    expect(endpointCell).toBeInTheDocument()
  })

  it('shows the actor display name with the raw id as a mono subline, and falls back to target_name for Target', async () => {
    vi.spyOn(globalThis, 'fetch').mockImplementation((input) => {
      const url = String(input).split('?')[0]
      if (url.endsWith('/console/capabilities'))
        return Promise.resolve(new Response(JSON.stringify(caps), { status: 200, headers: { 'content-type': 'application/json' } }))
      if (url.endsWith('/console/audit'))
        return Promise.resolve(
          new Response(
            JSON.stringify({
              org_id: 'o',
              events: [
                {
                  org_id: 'o',
                  actor_id: 'acct-alice',
                  actor_name: 'Alice Anderson',
                  actor_type: 'user',
                  action: 'key.create',
                  target: 'k1',
                  target_type: 'key',
                  target_name: 'ci-key',
                  detail: 'created api key',
                  at: '2026-06-25T08:00:00Z',
                },
              ],
            }),
            { status: 200, headers: { 'content-type': 'application/json' } },
          ),
        )
      return Promise.resolve(new Response(JSON.stringify({}), { status: 200, headers: { 'content-type': 'application/json' } }))
    })
    await renderAt('/audit', caps)
    await waitFor(() => expect(screen.getByText('Alice Anderson')).toBeInTheDocument())
    expect(screen.getByText('acct-alice')).toBeInTheDocument()
    expect(screen.getByText('ci-key')).toBeInTheDocument()
    // The raw action code is still visible for machine-grep parity.
    expect(screen.getByText('key.create')).toBeInTheDocument()
  })

  it('falls back to the raw actor id and target id when no name was resolved', async () => {
    await renderAt('/audit', caps)
    // The default beforeEach fixture has no actor_name/target_name.
    await waitFor(() => expect(screen.getByText('alice')).toBeInTheDocument())
    expect(screen.getByText('k1')).toBeInTheDocument()
  })

  it('Export control is an anchor linking to /console/audit/export', async () => {
    await renderAt('/audit', caps)
    await waitFor(() => {
      const link = screen.getByRole('link', { name: /export ndjson/i })
      expect(link).toBeInTheDocument()
      expect(link).toHaveAttribute('href', expect.stringContaining('/console/audit/export'))
    })
  })
})
