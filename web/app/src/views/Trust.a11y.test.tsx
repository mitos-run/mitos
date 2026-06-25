// Axe accessibility audit for the Trust view: no violations for both ownerships.
// Uses vitest-axe (^0.1.0) which wraps axe-core and provides Vitest-compatible
// matchers. Import path: 'vitest-axe' for axe(), 'vitest-axe/matchers' for the
// custom expect matcher.
import { describe, it, expect, vi, beforeEach } from 'vitest'
import { waitFor, screen } from '@testing-library/react'
import { axe } from 'vitest-axe'
import * as matchers from 'vitest-axe/matchers'
import { renderAt } from '../test/utils'
import type { Capabilities } from '../api'

expect.extend(matchers)

function makeCaps(ownership: 'self-hosted' | 'hosted'): Capabilities {
  return {
    edition: ownership === 'hosted' ? 'hosted' : 'community',
    billing: ownership === 'hosted',
    signup: false,
    teams: true,
    idp: 'oidc',
    orgSwitcher: ownership === 'hosted',
    secrets: { providers: ['kube'] },
    proof: true,
    ownership,
  }
}

function mockFetch(caps: Capabilities) {
  vi.spyOn(globalThis, 'fetch').mockImplementation((input) => {
    const url = String(input)
    if (url.endsWith('/console/capabilities')) {
      return Promise.resolve(
        new Response(JSON.stringify(caps), {
          status: 200,
          headers: { 'content-type': 'application/json' },
        }),
      )
    }
    return Promise.resolve(
      new Response(JSON.stringify({}), {
        status: 200,
        headers: { 'content-type': 'application/json' },
      }),
    )
  })
}

describe('Trust view accessibility', () => {
  beforeEach(() => vi.restoreAllMocks())

  it('has no axe violations for self-hosted ownership', async () => {
    const caps = makeCaps('self-hosted')
    mockFetch(caps)
    const { container } = await renderAt('/trust', caps)
    await waitFor(() =>
      expect(screen.getByText(/no external security review/i)).toBeInTheDocument(),
    )
    expect(await axe(container)).toHaveNoViolations()
  })

  it('has no axe violations for hosted ownership', async () => {
    const caps = makeCaps('hosted')
    mockFetch(caps)
    const { container } = await renderAt('/trust', caps)
    await waitFor(() =>
      expect(screen.getByText(/no external security review/i)).toBeInTheDocument(),
    )
    // Wait for compliance table to render.
    await waitFor(() =>
      expect(screen.getByRole('table', { name: /compliance artifacts/i })).toBeInTheDocument(),
    )
    expect(await axe(container)).toHaveNoViolations()
  })
})
