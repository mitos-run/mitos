import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, screen, waitFor } from '@testing-library/react'
import { App } from './App'

const caps = {
  edition: 'community', billing: false, signup: false, teams: true, idp: 'oidc',
  orgSwitcher: false, secrets: { providers: ['kube'] }, proof: true, ownership: 'self-hosted',
}

describe('App', () => {
  beforeEach(() => {
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
})
