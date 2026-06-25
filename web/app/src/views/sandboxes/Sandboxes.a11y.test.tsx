import { describe, it, expect, vi, beforeEach } from 'vitest'
import { waitFor, screen } from '@testing-library/react'
import { axe } from 'vitest-axe'
import * as matchers from 'vitest-axe/matchers'
import { renderAt } from '../../test/utils'
import type { Capabilities } from '../../api'

expect.extend(matchers)
const caps: Capabilities = {
  edition: 'community', billing: false, signup: false, teams: true, idp: 'oidc',
  orgSwitcher: false, secrets: { providers: ['kube'] }, proof: true, ownership: 'self-hosted',
}
beforeEach(() => {
  vi.spyOn(globalThis, 'fetch').mockImplementation((input) => {
    const url = String(input)
    if (url.endsWith('/console/capabilities')) return Promise.resolve(new Response(JSON.stringify(caps), { status: 200, headers: { 'content-type': 'application/json' } }))
    if (url.endsWith('/console/sandboxes')) return Promise.resolve(new Response(JSON.stringify({ org_id: 'o', sandboxes: [{ id: 's1', org_id: 'o', template: 't', node: 'n', phase: 'Running', vcpus: 1, mem_bytes: 1024, created_at: '' }] }), { status: 200, headers: { 'content-type': 'application/json' } }))
    return Promise.resolve(new Response(JSON.stringify({}), { status: 200, headers: { 'content-type': 'application/json' } }))
  })
})

describe('Sandboxes accessibility', () => {
  it('the list has no axe violations', async () => {
    const { container } = await renderAt('/sandboxes', caps)
    await waitFor(() => expect(screen.getByRole('table', { name: /live sandboxes/i })).toBeInTheDocument())
    expect(await axe(container)).toHaveNoViolations()
  })
})
