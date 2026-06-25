// Axe accessibility audit for the Retention view: no violations with inputs labelled,
// legal-hold checkbox labelled, and the form fully loaded.
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
  edition: 'community', billing: false, signup: false, teams: false, idp: 'oidc',
  orgSwitcher: false, secrets: { providers: ['kube'] }, proof: false, ownership: 'self-hosted',
}

const retentionPayload = {
  sandbox_metadata_days: 30,
  logs_days: 14,
  usage_days: 365,
  legal_hold: false,
}

beforeEach(() => {
  vi.spyOn(globalThis, 'fetch').mockImplementation((input) => {
    const url = String(input).split('?')[0]
    if (url.endsWith('/console/capabilities')) {
      return Promise.resolve(new Response(JSON.stringify(caps), { status: 200, headers: { 'content-type': 'application/json' } }))
    }
    if (url.endsWith('/console/retention')) {
      return Promise.resolve(new Response(JSON.stringify(retentionPayload), { status: 200, headers: { 'content-type': 'application/json' } }))
    }
    return Promise.resolve(new Response(JSON.stringify({}), { status: 200, headers: { 'content-type': 'application/json' } }))
  })
})

describe('Retention view accessibility', () => {
  it('has no axe violations on /retention', async () => {
    const { container } = await renderAt('/retention', caps)
    // Wait for the form to load: the sandbox-metadata input is a reliable sentinel.
    await waitFor(() => expect(screen.getByLabelText(/sandbox.?metadata/i)).toBeInTheDocument())
    // Confirm the legal-hold checkbox is labelled too.
    expect(screen.getByRole('checkbox', { name: /legal.?hold/i })).toBeInTheDocument()
    expect(await axe(container)).toHaveNoViolations()
  })
})
