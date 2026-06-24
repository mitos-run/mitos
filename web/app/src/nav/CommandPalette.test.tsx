import { describe, it, expect, vi, beforeEach } from 'vitest'
import { screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { fuzzyMatch } from './CommandPalette'
import { renderAt } from '../test/utils'
import type { Capabilities } from '../api'

describe('fuzzyMatch', () => {
  it('matches subsequence case-insensitively', () => {
    expect(fuzzyMatch('sbx', 'Sandboxes')).toBe(true)
    expect(fuzzyMatch('aud', 'Audit')).toBe(true)
    expect(fuzzyMatch('zzz', 'Audit')).toBe(false)
  })

  it('empty query matches everything', () => {
    expect(fuzzyMatch('', 'Anything')).toBe(true)
  })
})

const caps2: Capabilities = {
  edition: 'community', billing: false, signup: false, teams: true, idp: 'oidc',
  orgSwitcher: false, secrets: { providers: ['kube'] }, proof: true, ownership: 'self-hosted',
}

describe('CommandPalette behavior', () => {
  beforeEach(() => {
    vi.spyOn(globalThis, 'fetch').mockImplementation((input) => {
      const url = String(input)
      if (url.endsWith('/console/capabilities')) {
        return Promise.resolve(new Response(JSON.stringify(caps2), { status: 200, headers: { 'content-type': 'application/json' } }))
      }
      return Promise.resolve(new Response(JSON.stringify({ sandboxes: [], secrets: [] }), { status: 200, headers: { 'content-type': 'application/json' } }))
    })
  })

  it('opens on Cmd-K and filters', async () => {
    const user = userEvent.setup()
    await renderAt('/sandboxes', caps2)
    await waitFor(() => expect(screen.getByRole('link', { name: 'Sandboxes' })).toBeInTheDocument())
    await user.keyboard('{Meta>}k{/Meta}')
    const input = await screen.findByLabelText('Command palette input')
    await user.type(input, 'aud')
    expect(screen.getByRole('button', { name: /Audit/ })).toBeInTheDocument()
  })
})
