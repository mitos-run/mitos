import { describe, it, expect, vi, beforeEach } from 'vitest'
import { waitFor, screen } from '@testing-library/react'
import { renderAt } from '../test/utils'
import type { Capabilities } from '../api'

const caps: Capabilities = { edition: 'community', billing: false, signup: false, teams: true, idp: 'oidc', orgSwitcher: false, secrets: { providers: ['kube'] }, proof: true, ownership: 'self-hosted' }

beforeEach(() => {
  vi.spyOn(globalThis, 'fetch').mockImplementation((input) => {
    const url = String(input)
    if (url.endsWith('/console/capabilities')) return Promise.resolve(new Response(JSON.stringify(caps), { status: 200, headers: { 'content-type': 'application/json' } }))
    if (url.endsWith('/console/secrets')) return Promise.resolve(new Response(JSON.stringify({ org_id: 'o', secrets: [{ name: 'OPENAI_API_KEY', org_id: 'o', provider: 'kube', mode: 'copy-in', version: 2, fingerprint: 'ab12cd34' }] }), { status: 200, headers: { 'content-type': 'application/json' } }))
    return Promise.resolve(new Response(JSON.stringify({}), { status: 200, headers: { 'content-type': 'application/json' } }))
  })
})

describe('Secrets view', () => {
  it('lists secrets without revealing values', async () => {
    await renderAt('/secrets', caps)
    await waitFor(() => expect(screen.getByText('OPENAI_API_KEY')).toBeInTheDocument())
    expect(screen.getByText('kube')).toBeInTheDocument()
    expect(screen.getByText('ab12cd34')).toBeInTheDocument()
  })
})
