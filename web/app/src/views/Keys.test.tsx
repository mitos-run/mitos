// Behavior tests for the API keys view: create form, masked table, CopyOnce
// reveal (shown once in component state, never refetched), and optimistic revoke.
import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, fireEvent, waitFor, screen } from '@testing-library/react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { ToastProvider } from '../ui/Toast'
import { Keys } from './Keys'
import type { KeyView, CreateKeyResult } from '../api'

function wrap(ui: React.ReactElement) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  return render(
    <QueryClientProvider client={client}>
      <ToastProvider>{ui}</ToastProvider>
    </QueryClientProvider>,
  )
}

const baseKey: KeyView = {
  id: 'k1',
  name: 'ci-bot',
  prefix: 'mitos_live_ab12',
  scopes: ['sandboxes'],
  created_at: '2026-01-01T00:00:00Z',
  revoked: false,
}

const revokedKey: KeyView = {
  id: 'k2',
  name: 'old-key',
  prefix: 'mitos_live_zz99',
  scopes: ['read'],
  created_at: '2025-12-01T00:00:00Z',
  revoked: true,
  revoked_at: '2026-01-10T00:00:00Z',
}

beforeEach(() => {
  vi.spyOn(globalThis, 'fetch').mockImplementation((input) => {
    const url = String(input)
    if (url.endsWith('/console/keys') && !url.includes('revoke')) {
      return Promise.resolve(
        new Response(JSON.stringify({ keys: [baseKey, revokedKey] }), {
          status: 200,
          headers: { 'content-type': 'application/json' },
        }),
      )
    }
    return Promise.resolve(new Response(JSON.stringify({}), { status: 200, headers: { 'content-type': 'application/json' } }))
  })
})

describe('Keys view', () => {
  it('renders the create form with name input, scope checkboxes, TTL select, and submit button', async () => {
    wrap(<Keys />)
    await waitFor(() => expect(screen.getByLabelText(/name/i)).toBeInTheDocument())
    expect(screen.getByRole('checkbox', { name: /sandboxes/i })).toBeInTheDocument()
    expect(screen.getByRole('checkbox', { name: /read/i })).toBeInTheDocument()
    expect(screen.getByRole('combobox')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: /create key/i })).toBeInTheDocument()
  })

  it('renders the masked key table with accessible column headers', async () => {
    wrap(<Keys />)
    await waitFor(() => expect(screen.getByText('ci-bot')).toBeInTheDocument())
    expect(screen.getByRole('columnheader', { name: /name/i })).toBeInTheDocument()
    expect(screen.getByRole('columnheader', { name: /prefix/i })).toBeInTheDocument()
    expect(screen.getByRole('columnheader', { name: /scopes/i })).toBeInTheDocument()
    expect(screen.getByRole('columnheader', { name: /created/i })).toBeInTheDocument()
    expect(screen.getByText('mitos_live_ab12')).toBeInTheDocument()
    expect(screen.getByText('mitos_live_zz99')).toBeInTheDocument()
  })

  it('shows Revoke button only for non-revoked keys', async () => {
    wrap(<Keys />)
    await waitFor(() => expect(screen.getByText('ci-bot')).toBeInTheDocument())
    const revokeButtons = screen.getAllByRole('button', { name: /revoke/i })
    expect(revokeButtons.length).toBe(1)
  })

  it('shows CopyOnce after successful create and clears it on dismiss', async () => {
    const newKey: KeyView = { id: 'k3', name: 'new-key', prefix: 'mitos_live_cc11', scopes: ['sandboxes'], created_at: '2026-06-01T00:00:00Z', revoked: false }
    const createResult: CreateKeyResult = { org_id: 'o1', raw_key: 'mitos_live_secret_FULL_KEY', key: newKey }

    vi.spyOn(globalThis, 'fetch').mockImplementation((input, init) => {
      const url = String(input)
      if (url.endsWith('/console/keys') && init?.method === 'POST') {
        return Promise.resolve(
          new Response(JSON.stringify(createResult), { status: 200, headers: { 'content-type': 'application/json' } }),
        )
      }
      return Promise.resolve(
        new Response(JSON.stringify({ keys: [baseKey] }), { status: 200, headers: { 'content-type': 'application/json' } }),
      )
    })

    wrap(<Keys />)
    await waitFor(() => expect(screen.getByLabelText(/name/i)).toBeInTheDocument())
    fireEvent.change(screen.getByLabelText(/name/i), { target: { value: 'new-key' } })
    fireEvent.click(screen.getByRole('button', { name: /create key/i }))
    await waitFor(() => expect(screen.getByText('mitos_live_secret_FULL_KEY')).toBeInTheDocument())
    expect(screen.getByText(/shown once/i)).toBeInTheDocument()

    // Dismiss (the CopyOnce area should have a dismiss button or we check that
    // re-fetching the list does not bring the raw key back)
    const dismissBtn = screen.getByRole('button', { name: /dismiss/i })
    fireEvent.click(dismissBtn)
    await waitFor(() => expect(screen.queryByText('mitos_live_secret_FULL_KEY')).not.toBeInTheDocument())
  })

  it('shows loading skeleton while keys are fetching', () => {
    vi.spyOn(globalThis, 'fetch').mockImplementation(() => new Promise(() => {}))
    wrap(<Keys />)
    expect(screen.getByRole('status')).toBeInTheDocument()
  })

  it('shows empty state when there are no keys', async () => {
    vi.spyOn(globalThis, 'fetch').mockResolvedValue(
      new Response(JSON.stringify({ keys: [] }), { status: 200, headers: { 'content-type': 'application/json' } }),
    )
    wrap(<Keys />)
    await waitFor(() => expect(screen.getByText(/no api keys/i)).toBeInTheDocument())
  })

  it('optimistically marks a key revoked and shows a toast on success', async () => {
    vi.spyOn(globalThis, 'fetch').mockImplementation((input, init) => {
      const url = String(input)
      if (url.includes('/revoke') && init?.method === 'POST') {
        return Promise.resolve(new Response(JSON.stringify({}), { status: 200, headers: { 'content-type': 'application/json' } }))
      }
      return Promise.resolve(
        new Response(JSON.stringify({ keys: [baseKey] }), { status: 200, headers: { 'content-type': 'application/json' } }),
      )
    })
    wrap(<Keys />)
    await waitFor(() => expect(screen.getByText('ci-bot')).toBeInTheDocument())
    fireEvent.click(screen.getByRole('button', { name: /revoke/i }))
    await waitFor(() => expect(screen.getByRole('status')).toBeInTheDocument())
  })
})
