// Axe accessibility audit for the Audit view: no violations with populated
// retention, export, sinks, and event table data.
// Uses vitest-axe (^0.1.0) which wraps axe-core and provides Vitest-compatible
// matchers. Import path: 'vitest-axe' for axe(), 'vitest-axe/matchers' for the
// custom expect matcher.
import { describe, it, expect, vi, beforeEach } from 'vitest'
import { waitFor, screen } from '@testing-library/react'
import { axe } from 'vitest-axe'
import * as matchers from 'vitest-axe/matchers'
import { renderAt } from '../test/utils'
import type { Capabilities } from '../api'

expect.extend(matchers)

const caps: Capabilities = {
  edition: 'community', billing: false, signup: false, teams: true, idp: 'oidc',
  orgSwitcher: false, secrets: { providers: ['kube'] }, proof: true, ownership: 'self-hosted',
}

beforeEach(() => {
  vi.spyOn(globalThis, 'fetch').mockImplementation((input) => {
    const url = String(input).split('?')[0]
    if (url.endsWith('/console/capabilities')) {
      return Promise.resolve(new Response(JSON.stringify(caps), { status: 200, headers: { 'content-type': 'application/json' } }))
    }
    if (url.endsWith('/console/audit')) {
      return Promise.resolve(
        new Response(
          JSON.stringify({
            org_id: 'o',
            events: [
              { org_id: 'o', actor_id: 'alice', action: 'key.create', target: 'k1', detail: 'created api key', at: '2026-06-25T08:00:00Z' },
            ],
          }),
          { status: 200, headers: { 'content-type': 'application/json' } },
        ),
      )
    }
    if (url.endsWith('/console/audit/retention')) {
      return Promise.resolve(new Response(JSON.stringify({ days: 30 }), { status: 200, headers: { 'content-type': 'application/json' } }))
    }
    if (url.endsWith('/console/audit/sinks')) {
      return Promise.resolve(
        new Response(
          JSON.stringify({
            sinks: [
              { id: 's1', org_id: 'o', type: 'webhook', endpoint: 'https://siem.example/hook', enabled: true, created_at: '2026-06-01T00:00:00Z' },
            ],
          }),
          { status: 200, headers: { 'content-type': 'application/json' } },
        ),
      )
    }
    return Promise.resolve(new Response(JSON.stringify({}), { status: 200, headers: { 'content-type': 'application/json' } }))
  })
})

describe('Audit view accessibility', () => {
  it('has no axe violations with retention, export, sinks and event table', async () => {
    const { container } = await renderAt('/audit', caps)
    // Wait for all three data regions to render before auditing.
    await waitFor(() => expect(screen.getByRole('spinbutton', { name: /retention/i })).toBeInTheDocument())
    await waitFor(() => expect(screen.getByRole('link', { name: /export ndjson/i })).toBeInTheDocument())
    await waitFor(() => expect(screen.getByText('https://siem.example/hook')).toBeInTheDocument())
    expect(await axe(container)).toHaveNoViolations()
  })

  it('has no axe violations on the empty-sinks state', async () => {
    vi.spyOn(globalThis, 'fetch').mockImplementation((input) => {
      const url = String(input).split('?')[0]
      if (url.endsWith('/console/capabilities')) {
        return Promise.resolve(new Response(JSON.stringify(caps), { status: 200, headers: { 'content-type': 'application/json' } }))
      }
      if (url.endsWith('/console/audit')) {
        return Promise.resolve(new Response(JSON.stringify({ org_id: 'o', events: [] }), { status: 200, headers: { 'content-type': 'application/json' } }))
      }
      if (url.endsWith('/console/audit/retention')) {
        return Promise.resolve(new Response(JSON.stringify({ days: 90 }), { status: 200, headers: { 'content-type': 'application/json' } }))
      }
      if (url.endsWith('/console/audit/sinks')) {
        return Promise.resolve(new Response(JSON.stringify({ sinks: [] }), { status: 200, headers: { 'content-type': 'application/json' } }))
      }
      return Promise.resolve(new Response(JSON.stringify({}), { status: 200, headers: { 'content-type': 'application/json' } }))
    })
    const { container } = await renderAt('/audit', caps)
    await waitFor(() => expect(screen.getByRole('spinbutton', { name: /retention/i })).toBeInTheDocument())
    expect(await axe(container)).toHaveNoViolations()
  })
})
