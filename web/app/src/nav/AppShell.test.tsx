import { describe, it, expect, vi, beforeEach } from 'vitest'
import { waitFor, screen } from '@testing-library/react'
import { renderAt } from '../test/utils'
import type { Capabilities } from '../api'

const caps: Capabilities = {
  edition: 'community', billing: false, signup: false, teams: true, idp: 'oidc',
  orgSwitcher: false, secrets: { providers: ['kube'] }, proof: true, ownership: 'self-hosted',
}

beforeEach(() => {
  vi.spyOn(globalThis, 'fetch').mockImplementation((input) => {
    const url = String(input)
    if (url.endsWith('/console/capabilities')) {
      return Promise.resolve(new Response(JSON.stringify(caps), { status: 200, headers: { 'content-type': 'application/json' } }))
    }
    return Promise.resolve(new Response(JSON.stringify({ sandboxes: [], secrets: [] }), { status: 200, headers: { 'content-type': 'application/json' } }))
  })
})

describe('AppShell', () => {
  it('renders the group headers and the visible nav links', async () => {
    await renderAt('/sandboxes', caps)
    await waitFor(() => expect(screen.getByRole('link', { name: 'Sandboxes' })).toBeInTheDocument())
    expect(screen.getByText('Run')).toBeInTheDocument()
    expect(screen.getByText('Govern')).toBeInTheDocument()
    // billing is off -> no Billing link
    expect(screen.queryByRole('link', { name: 'Billing' })).not.toBeInTheDocument()
  })

  it('shows the self-hosted ownership badge', async () => {
    await renderAt('/sandboxes', caps)
    await waitFor(() => expect(screen.getByText('Self-hosted')).toBeInTheDocument())
  })
})
