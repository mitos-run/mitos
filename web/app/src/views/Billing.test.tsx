// Behavior tests for the Billing view: stat tiles, spend-cap form, ledger
// table, aria-live confirmation on a successful cap save, and add-credits UI.
import { describe, it, expect, vi, beforeEach, type MockInstance } from 'vitest'
import { fireEvent, waitFor, screen } from '@testing-library/react'
import { renderAt } from '../test/utils'
import { api, type Capabilities } from '../api'

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
  topup_available: true,
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

describe('Add credits section', () => {
  let openSpy: MockInstance
  let topupSpy: MockInstance

  beforeEach(() => {
    // The global beforeEach already sets up mockFetch() for capabilities + billing.
    // Spy on api.topupUrl directly so tests can assert exact cent amounts and
    // control promise resolution without depending on the fetch query string.
    topupSpy = vi.spyOn(api, 'topupUrl').mockResolvedValue('https://example/checkout')
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
    await waitFor(() => expect(topupSpy).toHaveBeenCalledWith(2500))
    expect(openSpy).toHaveBeenCalledWith('https://example/checkout', '_blank')
  })

  it('submitting a valid custom amount of 40 calls topupUrl(4000) and opens checkout', async () => {
    await renderAt('/billing', caps)
    const input = await waitFor(() => screen.getByLabelText(/custom amount/i))
    fireEvent.change(input, { target: { value: '40' } })
    fireEvent.click(screen.getByRole('button', { name: /add credits/i }))
    await waitFor(() => expect(topupSpy).toHaveBeenCalledWith(4000))
    expect(openSpy).toHaveBeenCalledWith('https://example/checkout', '_blank')
  })

  it('shows a calm validation message and does not open checkout for an empty custom amount', async () => {
    await renderAt('/billing', caps)
    await waitFor(() => screen.getByRole('button', { name: /add credits/i }))
    // Submit with no amount entered (empty field -> dollarsToCents returns 0 -> blocked).
    fireEvent.click(screen.getByRole('button', { name: /add credits/i }))
    await waitFor(() => expect(screen.getByRole('alert')).toBeInTheDocument())
    expect(screen.getByText(/valid dollar amount/i)).toBeInTheDocument()
    expect(topupSpy).not.toHaveBeenCalled()
    expect(openSpy).not.toHaveBeenCalled()
  })

  it('shows a calm error when topupUrl rejects and does not open checkout', async () => {
    topupSpy.mockRejectedValue(new Error('checkout unavailable'))
    await renderAt('/billing', caps)
    await waitFor(() => screen.getByRole('button', { name: '$10.00' }))
    fireEvent.click(screen.getByRole('button', { name: '$10.00' }))
    await waitFor(() => expect(screen.getByRole('alert')).toBeInTheDocument())
    expect(openSpy).not.toHaveBeenCalled()
  })

  // Fix 2: a second click while the first topupUrl call is still in-flight must not
  // fire a second call. The component sets disabled=true on all top-up controls for
  // the duration of the async call.
  it('a second click while topupUrl is in-flight does not fire a second call', async () => {
    let resolveTopup!: (url: string) => void
    const pendingPromise = new Promise<string>((resolve) => { resolveTopup = resolve })
    topupSpy.mockReturnValueOnce(pendingPromise)

    await renderAt('/billing', caps)
    const btn = await waitFor(() => screen.getByRole('button', { name: '$25.00' }))

    // First click starts the pending call.
    fireEvent.click(btn)

    // Wait for the button to become disabled while the call is in-flight.
    await waitFor(() => expect(btn).toBeDisabled())

    // Second click on the now-disabled button must not trigger another call.
    fireEvent.click(btn)

    // Resolve the first call and confirm topupUrl was only called once.
    resolveTopup('https://example/checkout')
    await waitFor(() => expect(openSpy).toHaveBeenCalledTimes(1))
    expect(topupSpy).toHaveBeenCalledTimes(1)
  })

  // Fix 3: a non-empty but invalid custom amount (negative, which is the JSDOM-
  // testable proxy for non-numeric input like "abc" that type="number" sanitizes
  // to "" in jsdom) must block submission and show the calm validation message.
  // This exercises the dollarsToCents null path (n < 0 returns null).
  it('entering an invalid non-empty custom amount does not call topupUrl and shows a calm message', async () => {
    await renderAt('/billing', caps)
    const input = await waitFor(() => screen.getByLabelText(/custom amount/i))
    // Use -5 to exercise dollarsToCents null path; JSDOM sanitizes "abc" to ""
    // for type="number" inputs (see spend-cap validation test for the same pattern).
    fireEvent.change(input, { target: { value: '-5' } })
    fireEvent.click(screen.getByRole('button', { name: /add credits/i }))
    await waitFor(() => expect(screen.getByRole('alert')).toBeInTheDocument())
    expect(screen.getByText(/valid dollar amount/i)).toBeInTheDocument()
    expect(topupSpy).not.toHaveBeenCalled()
    expect(openSpy).not.toHaveBeenCalled()
  })

  it('shows a calm not-available note and no add-credits controls when topup_available is false', async () => {
    vi.spyOn(globalThis, 'fetch').mockImplementation((input, init) => {
      const url = String(input).split('?')[0]
      const method = (init?.method ?? 'GET').toUpperCase()
      if (url.endsWith('/console/capabilities')) {
        return Promise.resolve(new Response(JSON.stringify(caps), { status: 200, headers: { 'content-type': 'application/json' } }))
      }
      if (url.endsWith('/console/billing') && method === 'GET') {
        return Promise.resolve(
          new Response(
            JSON.stringify({ ...billingPayload, topup_available: false }),
            { status: 200, headers: { 'content-type': 'application/json' } },
          ),
        )
      }
      return Promise.resolve(new Response(JSON.stringify({}), { status: 200, headers: { 'content-type': 'application/json' } }))
    })
    await renderAt('/billing', caps)
    await waitFor(() => expect(screen.getByText(/adding credits is not available yet/i)).toBeInTheDocument())
    // Tier buttons and Add credits submit button must not be rendered.
    expect(screen.queryByRole('button', { name: '$10.00' })).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /add credits/i })).not.toBeInTheDocument()
  })

  it('shows the active add-credits controls when topup_available is true', async () => {
    // billingPayload already sets topup_available: true; the global beforeEach
    // uses mockFetch() which serves it, so no additional setup is needed.
    await renderAt('/billing', caps)
    await waitFor(() => expect(screen.getByRole('button', { name: '$10.00' })).toBeInTheDocument())
    expect(screen.getByRole('button', { name: /add credits/i })).toBeInTheDocument()
    expect(screen.queryByText(/adding credits is not available yet/i)).not.toBeInTheDocument()
  })
})
