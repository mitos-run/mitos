import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, screen, waitFor } from '@testing-library/react'
import { App } from './App'
import { queryClient } from './data/query'

const caps = {
  edition: 'community', billing: false, signup: false, teams: true, idp: 'oidc',
  orgSwitcher: false, secrets: { providers: ['kube'] }, proof: true, ownership: 'self-hosted',
}

describe('App', () => {
  beforeEach(() => {
    // The module-level QueryClient caches capabilities forever; clear it so
    // each test controls its own fetch behavior.
    queryClient.clear()
    vi.spyOn(globalThis, 'fetch').mockImplementation((input) => {
      const url = String(input)
      if (url.endsWith('/console/capabilities')) {
        return Promise.resolve(new Response(JSON.stringify(caps), { status: 200, headers: { 'content-type': 'application/json' } }))
      }
      return Promise.resolve(new Response(JSON.stringify({ sandboxes: [], secrets: [], instruments: {} }), { status: 200, headers: { 'content-type': 'application/json' } }))
    })
  })

  it('boots, fetches capabilities, and renders the shell nav', async () => {
    render(<App />)
    await waitFor(() => expect(screen.getByRole('link', { name: 'Sandboxes' })).toBeInTheDocument())
    expect(screen.getByText('Run')).toBeInTheDocument()
  })

  it('shows the skeleton status region, not bare text, while capabilities load', () => {
    // A fetch that never resolves keeps the capabilities query in flight.
    vi.spyOn(globalThis, 'fetch').mockImplementation(() => new Promise(() => {}))
    render(<App />)
    expect(screen.getByRole('status')).toHaveAccessibleName(/loading/i)
    expect(screen.queryByText('loading...')).not.toBeInTheDocument()
  })
})
