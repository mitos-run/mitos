// Axe accessibility audit for B2b views: no violations on Keys and Secrets.
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
    if (url.endsWith('/console/keys')) {
      return Promise.resolve(new Response(JSON.stringify({ org_id: 'o', keys: [] }), { status: 200, headers: { 'content-type': 'application/json' } }))
    }
    if (url.endsWith('/console/secrets')) {
      return Promise.resolve(new Response(JSON.stringify({ org_id: 'o', secrets: [] }), { status: 200, headers: { 'content-type': 'application/json' } }))
    }
    return Promise.resolve(new Response(JSON.stringify({}), { status: 200, headers: { 'content-type': 'application/json' } }))
  })
})

describe('B2b views accessibility', () => {
  it('Keys has no axe violations', async () => {
    const { container } = await renderAt('/keys', caps)
    await waitFor(() => expect(screen.getByRole('button', { name: /create key/i })).toBeInTheDocument())
    expect(await axe(container)).toHaveNoViolations()
  })

  it('Secrets has no axe violations', async () => {
    const { container } = await renderAt('/secrets', caps)
    await waitFor(() => expect(screen.getByRole('button', { name: /create/i })).toBeInTheDocument())
    expect(await axe(container)).toHaveNoViolations()
  })
})
