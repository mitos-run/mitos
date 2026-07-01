// Behavior tests for the Billing view: stat tiles, spend-cap form, ledger
// table, and aria-live confirmation on a successful cap save.
import { describe, it, expect, vi, beforeEach } from 'vitest'
import { fireEvent, waitFor, screen } from '@testing-library/react'
import { renderAt } from '../test/utils'
import type { Capabilities } from '../api'

const caps: Capabilities = {
  edition: 'hosted',
  billing: true,
  signup: false,
  teams: false,
  idp: 'oidc',
  orgSwitcher: false,
  secrets: { providers: ['kube'] },
  proof: false,
  ownership: 'hosted',
}

const billingPayload = {
  org_id: 'org-1',
  status: 'active',
  balance_cents: 5000,
  spend_cents: 1200,
  soft_cap_cents: 8000,
  hard_cap_cents: 10000,
  ledger_entries: [
    { ts: '2026-06-01T00:00:00Z', cents: 5000, reason: 'signup credit' },
  ],
}

function mockFetch(postResponse?: object) {
  vi.spyOn(globalThis, 'fetch').mockImplementation((input, init) => {
    const url = String(input).split('?')[0]
    const method = (init?.method ?? 'GET').toUpperCase()

    if (url.endsWith('/console/capabilities')) {
      return Promise.resolve(
        new Response(JSON.stringify(caps), { status: 200, headers: { 'content-type': 'application/json' } }),
      )
    }
    if (url.endsWith('/console/billing') && method === 'GET') {
      return Promise.resolve(
        new Response(JSON.stringify(billingPayload), { status: 200, headers: { 'content-type': 'application/json' } }),
      )
    }
    if (url.endsWith('/console/billing/spend-cap') && method === 'POST') {
      return Promise.resolve(
        new Response(
          JSON.stringify(postResponse ?? { org_id: 'org-1', soft_cap_cents: 2000, hard_cap_cents: 5000 }),
          { status: 200, headers: { 'content-type': 'application/json' } },
        ),
      )
    }
    return Promise.resolve(
      new Response(JSON.stringify({}), { status: 200, headers: { 'content-type': 'application/json' } }),
    )
  })
}

beforeEach(() => {
  mockFetch()
})

describe('Billing view', () => {
  it('renders stat tiles for balance, spend, and caps', async () => {
    await renderAt('/billing', caps)
    await waitFor(() => expect(screen.getByText('Balance')).toBeInTheDocument())
    expect(screen.getByText('Spend')).toBeInTheDocument()
    expect(screen.getByText('Soft cap')).toBeInTheDocument()
    expect(screen.getByText('Hard cap')).toBeInTheDocument()
  })

  it('renders the ledger table with the entry reason', async () => {
    await renderAt('/billing', caps)
    await waitFor(() => expect(screen.getByText('signup credit')).toBeInTheDocument())
  })

  it('renders the set-spend-cap form with soft and hard inputs', async () => {
    await renderAt('/billing', caps)
    await waitFor(() => expect(screen.getByLabelText(/soft cap/i)).toBeInTheDocument())
    expect(screen.getByLabelText(/hard cap/i)).toBeInTheDocument()
    expect(screen.getByRole('button', { name: /save spend cap/i })).toBeInTheDocument()
  })

  it('posts the caps to /console/billing/spend-cap and shows confirmation', async () => {
    const calls: { url: string; body: object }[] = []
    vi.spyOn(globalThis, 'fetch').mockImplementation((input, init) => {
      const url = String(input).split('?')[0]
      const method = (init?.method ?? 'GET').toUpperCase()

      if (url.endsWith('/console/capabilities')) {
        return Promise.resolve(
          new Response(JSON.stringify(caps), { status: 200, headers: { 'content-type': 'application/json' } }),
        )
      }
      if (url.endsWith('/console/billing') && method === 'GET') {
        return Promise.resolve(
          new Response(JSON.stringify(billingPayload), { status: 200, headers: { 'content-type': 'application/json' } }),
        )
      }
      if (url.endsWith('/console/billing/spend-cap') && method === 'POST') {
        const body = JSON.parse((init?.body as string) ?? '{}') as object
        calls.push({ url, body })
        return Promise.resolve(
          new Response(
            JSON.stringify({ org_id: 'org-1', soft_cap_cents: 2000, hard_cap_cents: 5000 }),
            { status: 200, headers: { 'content-type': 'application/json' } },
          ),
        )
      }
      return Promise.resolve(
        new Response(JSON.stringify({}), { status: 200, headers: { 'content-type': 'application/json' } }),
      )
    })

    await renderAt('/billing', caps)

    const softInput = await waitFor(() => screen.getByLabelText(/soft cap/i))
    const hardInput = screen.getByLabelText(/hard cap/i)

    fireEvent.change(softInput, { target: { value: '20' } })
    fireEvent.change(hardInput, { target: { value: '50' } })

    fireEvent.click(screen.getByRole('button', { name: /save spend cap/i }))

    // The confirmation text should appear after the POST succeeds.
    await waitFor(() => expect(screen.getByRole('status')).toBeInTheDocument())
    expect(screen.getByText(/spend cap saved/i)).toBeInTheDocument()

    // The POST body must carry integer cents (20 dollars = 2000 cents).
    expect(calls.length).toBeGreaterThanOrEqual(1)
    const last = calls[calls.length - 1]
    expect(last.body).toEqual({ soft_cents: 2000, hard_cents: 5000 })
  })

  it('shows an empty ledger state when there are no entries', async () => {
    vi.spyOn(globalThis, 'fetch').mockImplementation((input, init) => {
      const url = String(input).split('?')[0]
      const method = (init?.method ?? 'GET').toUpperCase()
      if (url.endsWith('/console/capabilities')) {
        return Promise.resolve(
          new Response(JSON.stringify(caps), { status: 200, headers: { 'content-type': 'application/json' } }),
        )
      }
      if (url.endsWith('/console/billing') && method === 'GET') {
        return Promise.resolve(
          new Response(
            JSON.stringify({ ...billingPayload, ledger_entries: [] }),
            { status: 200, headers: { 'content-type': 'application/json' } },
          ),
        )
      }
      return Promise.resolve(
        new Response(JSON.stringify({}), { status: 200, headers: { 'content-type': 'application/json' } }),
      )
    })
    await renderAt('/billing', caps)
    await waitFor(() => expect(screen.getByText(/no ledger entries/i)).toBeInTheDocument())
  })
})
