// Axe accessibility audit for AppShell: no violations in either drawer state.
// Uses vitest-axe (^0.1.0) which wraps axe-core and provides Vitest-compatible
// matchers. Import path: 'vitest-axe' for axe(), 'vitest-axe/matchers' for the
// custom expect matcher.
import { describe, it, expect, vi, beforeEach } from 'vitest'
import { waitFor, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { axe } from 'vitest-axe'
import * as matchers from 'vitest-axe/matchers'
import { renderAt } from '../test/utils'
import type { Capabilities } from '../api'

expect.extend(matchers)

vi.mock('../data/account-settings', () => ({
  useAccount: () => ({ data: { display_name: 'Test User', email: 'test@example.com', memberships: [] } }),
  useSignOut: () => ({ mutate: vi.fn(), isPending: false }),
}))

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

describe('AppShell accessibility', () => {
  it('has no axe violations with the drawer closed', async () => {
    const { container } = await renderAt('/sandboxes', caps)
    await waitFor(() => expect(screen.getByRole('link', { name: 'Sandboxes' })).toBeInTheDocument())
    expect(await axe(container)).toHaveNoViolations()
  })

  it('has no axe violations with the drawer open', async () => {
    const user = userEvent.setup()
    const { container } = await renderAt('/sandboxes', caps)
    await user.click(await screen.findByRole('button', { name: /open navigation menu/i }))
    expect(await axe(container)).toHaveNoViolations()
  })
})
