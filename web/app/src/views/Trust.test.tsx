import { describe, it, expect, vi, beforeEach } from 'vitest'
import { waitFor, screen } from '@testing-library/react'
import { renderAt } from '../test/utils'
import type { Capabilities } from '../api'

function caps(ownership: 'self-hosted' | 'hosted'): Capabilities {
  return { edition: ownership === 'hosted' ? 'hosted' : 'community', billing: ownership === 'hosted', signup: false, teams: true, idp: 'oidc', orgSwitcher: ownership === 'hosted', secrets: { providers: ['kube'] }, proof: true, ownership }
}

function mockCaps(c: Capabilities) {
  vi.spyOn(globalThis, 'fetch').mockImplementation((input) => {
    const url = String(input)
    if (url.endsWith('/console/capabilities')) return Promise.resolve(new Response(JSON.stringify(c), { status: 200, headers: { 'content-type': 'application/json' } }))
    return Promise.resolve(new Response(JSON.stringify({}), { status: 200, headers: { 'content-type': 'application/json' } }))
  })
}

describe('Trust view', () => {
  beforeEach(() => vi.restoreAllMocks())

  it('always shows the honest no-external-review banner and the threat-model link', async () => {
    mockCaps(caps('self-hosted'))
    await renderAt('/trust', caps('self-hosted'))
    await waitFor(() => expect(screen.getByText(/no external security review/i)).toBeInTheDocument())
    expect(screen.getByRole('link', { name: /threat model/i })).toHaveAttribute('href', expect.stringContaining('threat-model.md'))
  })

  it('shows the self-host residency posture for self-hosted', async () => {
    mockCaps(caps('self-hosted'))
    await renderAt('/trust', caps('self-hosted'))
    await waitFor(() => expect(screen.getByText(/your infrastructure/i)).toBeInTheDocument())
  })

  it('shows hosted compliance framed honestly (not certified) for hosted', async () => {
    mockCaps(caps('hosted'))
    await renderAt('/trust', caps('hosted'))
    await waitFor(() => expect(screen.getByText(/SOC2/i)).toBeInTheDocument())
    // honest framing: no "certified"/"compliant" claim text
    expect(screen.queryByText(/\bcertified\b/i)).not.toBeInTheDocument()
  })
})
