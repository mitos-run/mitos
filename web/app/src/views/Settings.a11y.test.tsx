// Axe accessibility audit for the Settings view: no violations on Profile and Security tabs.
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
import type { AccountView, SessionView } from '../api'

expect.extend(matchers)

const caps: Capabilities = {
  edition: 'community', billing: false, signup: false, teams: true, idp: 'oidc',
  orgSwitcher: false, secrets: { providers: ['kube'] }, proof: true, ownership: 'self-hosted',
}

const account: AccountView = {
  account_id: 'acc-1',
  email: 'test@example.com',
  display_name: 'Test User',
  timezone: 'UTC',
  locale: 'en',
  memberships: [{
    account_id: 'acc-1',
    org_id: 'org-1',
    role: 'admin',
    created_at: '2024-01-01T00:00:00Z',
  }],
}

const sessions: SessionView[] = [{
  id: 'sess-1',
  label: 'Web browser',
  created_at: '2024-01-01T00:00:00Z',
  current: true,
}]

beforeEach(() => {
  vi.spyOn(globalThis, 'fetch').mockImplementation((input) => {
    const url = String(input)
    if (url.endsWith('/console/capabilities')) {
      return Promise.resolve(new Response(JSON.stringify(caps), { status: 200, headers: { 'content-type': 'application/json' } }))
    }
    if (url.endsWith('/console/account/sessions')) {
      return Promise.resolve(new Response(JSON.stringify({ sessions }), { status: 200, headers: { 'content-type': 'application/json' } }))
    }
    if (url.endsWith('/console/account')) {
      return Promise.resolve(new Response(JSON.stringify(account), { status: 200, headers: { 'content-type': 'application/json' } }))
    }
    return Promise.resolve(new Response(JSON.stringify({}), { status: 200, headers: { 'content-type': 'application/json' } }))
  })
})

describe('Settings accessibility', () => {
  it('has no axe violations on the Profile tab', async () => {
    const { container } = await renderAt('/settings', caps)
    await waitFor(() => expect(screen.getByRole('heading', { name: 'Settings' })).toBeInTheDocument())
    // Wait for profile data to load (display name input should be visible).
    await waitFor(() => expect(screen.getByLabelText('Display name')).toBeInTheDocument())
    expect(await axe(container)).toHaveNoViolations()
  })

  it('has no axe violations on the Security tab', async () => {
    const user = userEvent.setup()
    const { container } = await renderAt('/settings', caps)
    await waitFor(() => expect(screen.getByRole('heading', { name: 'Settings' })).toBeInTheDocument())
    await user.click(screen.getByRole('tab', { name: 'Security' }))
    // Wait for sessions table to render.
    await waitFor(() => expect(screen.getByRole('table', { name: 'Sessions' })).toBeInTheDocument())
    expect(await axe(container)).toHaveNoViolations()
  })
})
