// Axe accessibility audit for the Overview (Instruments) view: no violations
// with populated proof tiles, available-credit band, running-sandboxes panel,
// and recent-activity panel.
// Uses vitest-axe (^0.1.0) which wraps axe-core and provides Vitest-compatible
// matchers. Import path: 'vitest-axe' for axe(), 'vitest-axe/matchers' for the
// custom expect matcher.
import { describe, it, expect, vi, beforeEach } from 'vitest'
import { screen } from '@testing-library/react'
import { axe } from 'vitest-axe'
import * as matchers from 'vitest-axe/matchers'
import { renderAt } from '../test/utils'
import type { Capabilities } from '../api'

expect.extend(matchers)

const caps: Capabilities = {
  edition: 'community', billing: true, signup: false, teams: true, idp: 'oidc',
  orgSwitcher: false, secrets: { providers: ['kube'] }, proof: true, ownership: 'self-hosted',
}

const instrumentsData = {
  org_id: 'o1',
  activate_p50_ms: 27,
  activate_p99_ms: 41,
  forks_served: 10,
  cow_savings_bytes: 2415919104,
  marginal_bytes_per_fork: 3145728,
}

const sandboxesData = [
  { id: 'sb-aaa', org_id: 'o1', template: 'python', node: 'w1', phase: 'Running', vcpus: 2, mem_bytes: 1073741824, created_at: '2026-01-01T00:00:00Z' },
  { id: 'sb-bbb', org_id: 'o1', template: 'python', node: 'w1', phase: 'Running', vcpus: 2, mem_bytes: 1073741824, created_at: '2026-01-01T00:00:00Z' },
  { id: 'sb-ccc', org_id: 'o1', template: 'python', node: 'w1', phase: 'Stopped', vcpus: 2, mem_bytes: 1073741824, created_at: '2026-01-01T00:00:00Z' },
]

const auditData = [
  { org_id: 'o1', actor_id: 'alice', action: 'fork', target: 'sb-aaa', detail: '', at: '2026-01-01T10:00:00Z' },
  { org_id: 'o1', actor_id: 'bob', action: 'terminate', target: 'sb-bbb', detail: '', at: '2026-01-01T09:00:00Z' },
]

const billingData = {
  org_id: 'o1',
  status: 'active',
  balance_cents: 5000,
  spend_cents: 1234,
  soft_cap_cents: 0,
  hard_cap_cents: 0,
  ledger_entries: [],
}

beforeEach(() => {
  vi.spyOn(globalThis, 'fetch').mockImplementation((input) => {
    const url = String(input).split('?')[0]
    if (url.endsWith('/console/capabilities')) {
      return Promise.resolve(new Response(JSON.stringify(caps), { status: 200, headers: { 'content-type': 'application/json' } }))
    }
    if (url.endsWith('/console/instruments')) {
      return Promise.resolve(new Response(JSON.stringify(instrumentsData), { status: 200, headers: { 'content-type': 'application/json' } }))
    }
    if (url.endsWith('/console/sandboxes')) {
      return Promise.resolve(new Response(JSON.stringify({ sandboxes: sandboxesData }), { status: 200, headers: { 'content-type': 'application/json' } }))
    }
    if (url.endsWith('/console/billing')) {
      return Promise.resolve(new Response(JSON.stringify(billingData), { status: 200, headers: { 'content-type': 'application/json' } }))
    }
    if (url.endsWith('/console/audit')) {
      return Promise.resolve(new Response(JSON.stringify({ events: auditData }), { status: 200, headers: { 'content-type': 'application/json' } }))
    }
    if (url.endsWith('/console/first-activity')) {
      return Promise.resolve(new Response(JSON.stringify({ active: false }), { status: 200, headers: { 'content-type': 'application/json' } }))
    }
    return Promise.resolve(new Response(JSON.stringify({}), { status: 200, headers: { 'content-type': 'application/json' } }))
  })
})

describe('Overview accessibility', () => {
  it('has no axe violations', async () => {
    const { container } = await renderAt('/', caps)
    // Wait for data to load so axe audits the populated view, not the skeleton.
    await screen.findByRole('heading', { name: /overview/i })
    expect(await axe(container)).toHaveNoViolations()
  })
})
