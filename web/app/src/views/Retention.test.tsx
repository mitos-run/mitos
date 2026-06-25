// Behavior tests for the Retention view: number inputs for sandbox-metadata,
// logs, and usage retention days; a legal-hold checkbox; Save -> toast; the
// honest GC-enforcement note.
import { describe, it, expect, vi, beforeEach } from 'vitest'
import { fireEvent, waitFor, screen } from '@testing-library/react'
import { renderAt } from '../test/utils'
import type { Capabilities } from '../api'

const caps: Capabilities = {
  edition: 'community',
  billing: false,
  signup: false,
  teams: false,
  idp: 'oidc',
  orgSwitcher: false,
  secrets: { providers: ['kube'] },
  proof: false,
  ownership: 'self-hosted',
}

const retentionPayload = {
  sandbox_metadata_days: 30,
  logs_days: 14,
  usage_days: 365,
  legal_hold: false,
}

function mockFetch(putResponse?: object) {
  vi.spyOn(globalThis, 'fetch').mockImplementation((input, init) => {
    const url = String(input).split('?')[0]
    const method = (init?.method ?? 'GET').toUpperCase()

    if (url.endsWith('/console/capabilities')) {
      return Promise.resolve(
        new Response(JSON.stringify(caps), { status: 200, headers: { 'content-type': 'application/json' } }),
      )
    }
    if (url.endsWith('/console/retention') && method === 'GET') {
      return Promise.resolve(
        new Response(JSON.stringify(retentionPayload), { status: 200, headers: { 'content-type': 'application/json' } }),
      )
    }
    if (url.endsWith('/console/retention') && method === 'PUT') {
      return Promise.resolve(
        new Response(JSON.stringify(putResponse ?? retentionPayload), { status: 200, headers: { 'content-type': 'application/json' } }),
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

describe('Retention view', () => {
  it('renders the sandbox-metadata days input with the fetched value', async () => {
    await renderAt('/retention', caps)
    const input = await waitFor(() => screen.getByLabelText(/sandbox.?metadata/i))
    expect(input.tagName).toBe('INPUT')
    expect((input as HTMLInputElement).value).toBe('30')
  })

  it('renders the logs days input with the fetched value', async () => {
    await renderAt('/retention', caps)
    const input = await waitFor(() => screen.getByLabelText(/logs/i))
    expect(input.tagName).toBe('INPUT')
    expect((input as HTMLInputElement).value).toBe('14')
  })

  it('renders the usage days input with the fetched value', async () => {
    await renderAt('/retention', caps)
    const input = await waitFor(() => screen.getByLabelText(/usage/i))
    expect(input.tagName).toBe('INPUT')
    expect((input as HTMLInputElement).value).toBe('365')
  })

  it('renders the legal-hold checkbox unchecked when legal_hold is false', async () => {
    await renderAt('/retention', caps)
    const checkbox = await waitFor(() => screen.getByRole('checkbox', { name: /legal.?hold/i }))
    expect((checkbox as HTMLInputElement).checked).toBe(false)
  })

  it('toggling legal hold and saving calls PUT /console/retention with updated value', async () => {
    const calls: { url: string; body: object }[] = []
    vi.spyOn(globalThis, 'fetch').mockImplementation((input, init) => {
      const url = String(input).split('?')[0]
      const method = (init?.method ?? 'GET').toUpperCase()

      if (url.endsWith('/console/capabilities')) {
        return Promise.resolve(
          new Response(JSON.stringify(caps), { status: 200, headers: { 'content-type': 'application/json' } }),
        )
      }
      if (url.endsWith('/console/retention') && method === 'GET') {
        return Promise.resolve(
          new Response(JSON.stringify(retentionPayload), { status: 200, headers: { 'content-type': 'application/json' } }),
        )
      }
      if (url.endsWith('/console/retention') && method === 'PUT') {
        const body = JSON.parse((init?.body as string) ?? '{}') as object
        calls.push({ url, body })
        const echoed = { ...retentionPayload, ...body }
        return Promise.resolve(
          new Response(JSON.stringify(echoed), { status: 200, headers: { 'content-type': 'application/json' } }),
        )
      }
      return Promise.resolve(
        new Response(JSON.stringify({}), { status: 200, headers: { 'content-type': 'application/json' } }),
      )
    })

    await renderAt('/retention', caps)

    // Wait for the form to load
    await waitFor(() => screen.getByRole('checkbox', { name: /legal.?hold/i }))

    // Toggle legal hold on
    const checkbox = screen.getByRole('checkbox', { name: /legal.?hold/i })
    fireEvent.click(checkbox)
    expect((checkbox as HTMLInputElement).checked).toBe(true)

    // Click Save
    const saveButton = screen.getByRole('button', { name: /save/i })
    fireEvent.click(saveButton)

    // The PUT should be called with legal_hold: true
    await waitFor(() => expect(calls.length).toBeGreaterThanOrEqual(1))
    const lastCall = calls[calls.length - 1]
    expect(lastCall.url).toContain('/console/retention')
    expect((lastCall.body as { legal_hold: boolean }).legal_hold).toBe(true)
  })

  it('shows a "0 = keep forever" hint on one of the number inputs', async () => {
    await renderAt('/retention', caps)
    await waitFor(() => screen.getByLabelText(/sandbox.?metadata/i))
    expect(screen.getByText(/0.{0,10}keep forever/i)).toBeInTheDocument()
  })

  it('shows an honest note about GC enforcement', async () => {
    await renderAt('/retention', caps)
    await waitFor(() => screen.getByLabelText(/sandbox.?metadata/i))
    const gcNodes = screen.getAllByText(/garbage collector/i)
    expect(gcNodes.length).toBeGreaterThanOrEqual(1)
  })
})
