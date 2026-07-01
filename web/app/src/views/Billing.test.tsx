// Behavior tests for the Billing view: stat tiles, spend-cap form, ledger
// table, aria-live confirmation on a successful cap save, and add-credits UI.
import { describe, it, expect, vi, beforeEach, type MockInstance } from 'vitest'
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
          JSON.stringify(postResponse ?? { org_id: 'org-1' }),
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
            JSON.stringify({ org_id: 'org-1' }),
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

  it('blocks submit and shows a validation message when an amount is negative', async () => {
    // A type="number" input does not pass 'abc' through the change event in JSDOM
    // (the browser sanitizes non-numeric text to ""). A negative number (-5) CAN
    // be typed into a number input and is the real invalid-dollar-amount scenario:
    // dollarsToCents('-5') returns null, which must block submit and show an alert.
    const fetchSpy = vi.spyOn(globalThis, 'fetch')
    await renderAt('/billing', caps)
    const softInput = await waitFor(() => screen.getByLabelText(/soft cap/i))
    fireEvent.change(softInput, { target: { value: '-5' } })
    fireEvent.click(screen.getByRole('button', { name: /save spend cap/i }))
    // A validation alert must appear and no POST must have been made to the cap endpoint.
    await waitFor(() => expect(screen.getByRole('alert')).toBeInTheDocument())
    expect(screen.getByText(/valid dollar amount/i)).toBeInTheDocument()
    const capCalls = fetchSpy.mock.calls.filter(([input, init]) => {
      const url = String(input).split('?')[0]
      const method = ((init as RequestInit | undefined)?.method ?? 'GET').toUpperCase()
      return url.endsWith('/console/billing/spend-cap') && method === 'POST'
    })
    expect(capCalls.length).toBe(0)
  })

  it('shows an error alert when the spend-cap mutation fails', async () => {
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
            JSON.stringify({ error: { code: 'invalid_input', message: 'soft_cents must not exceed hard_cents', remediation: 'Correct the value.' } }),
            { status: 400, headers: { 'content-type': 'application/json' } },
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
    fireEvent.change(softInput, { target: { value: '50' } })
    fireEvent.change(hardInput, { target: { value: '20' } })
    fireEvent.click(screen.getByRole('button', { name: /save spend cap/i }))
    await waitFor(() => expect(screen.getByRole('alert')).toBeInTheDocument())
    expect(screen.getByText(/could not be saved/i)).toBeInTheDocument()
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

// Helper: mock fetch including the topup endpoint.
// When topupOk is false the topup endpoint returns 400 (not configured / invalid).
function mockFetchWithTopup(topupOk = true) {
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
    if (url.endsWith('/console/billing/topup') && method === 'GET') {
      if (!topupOk) {
        return Promise.resolve(new Response('', { status: 400 }))
      }
      return Promise.resolve(
        new Response(JSON.stringify({ url: 'https://example/checkout' }), { status: 200, headers: { 'content-type': 'application/json' } }),
      )
    }
    return Promise.resolve(
      new Response(JSON.stringify({}), { status: 200, headers: { 'content-type': 'application/json' } }),
    )
  })
}

describe('Add credits section', () => {
  let openSpy: MockInstance

  beforeEach(() => {
    mockFetchWithTopup()
    openSpy = vi.spyOn(window, 'open').mockImplementation(() => null)
  })

  it('renders the four preset tier buttons', async () => {
    await renderAt('/billing', caps)
    await waitFor(() => expect(screen.getByRole('button', { name: '$10.00' })).toBeInTheDocument())
    expect(screen.getByRole('button', { name: '$25.00' })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: '$50.00' })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: '$100.00' })).toBeInTheDocument()
  })

  it('clicking the $25 preset calls topupUrl(2500) and opens the checkout url', async () => {
    await renderAt('/billing', caps)
    await waitFor(() => screen.getByRole('button', { name: '$25.00' }))
    fireEvent.click(screen.getByRole('button', { name: '$25.00' }))
    await waitFor(() => expect(openSpy).toHaveBeenCalledWith('https://example/checkout', '_blank'))
  })

  it('submitting a valid custom amount of 40 calls topupUrl(4000) and opens checkout', async () => {
    await renderAt('/billing', caps)
    const input = await waitFor(() => screen.getByLabelText(/custom amount/i))
    fireEvent.change(input, { target: { value: '40' } })
    fireEvent.click(screen.getByRole('button', { name: /add credits/i }))
    await waitFor(() => expect(openSpy).toHaveBeenCalledWith('https://example/checkout', '_blank'))
  })

  it('shows a calm validation message and does not open checkout for an empty custom amount', async () => {
    await renderAt('/billing', caps)
    await waitFor(() => screen.getByRole('button', { name: /add credits/i }))
    // submit with no amount entered (empty field -> dollarsToCents returns 0 -> blocked)
    fireEvent.click(screen.getByRole('button', { name: /add credits/i }))
    await waitFor(() => expect(screen.getByRole('alert')).toBeInTheDocument())
    expect(screen.getByText(/valid dollar amount/i)).toBeInTheDocument()
    expect(openSpy).not.toHaveBeenCalled()
  })

  it('shows a calm error when topupUrl rejects and does not open checkout', async () => {
    mockFetchWithTopup(false)
    await renderAt('/billing', caps)
    await waitFor(() => screen.getByRole('button', { name: '$10.00' }))
    fireEvent.click(screen.getByRole('button', { name: '$10.00' }))
    await waitFor(() => expect(screen.getByRole('alert')).toBeInTheDocument())
    expect(openSpy).not.toHaveBeenCalled()
  })
})
