// Behavior tests for the Settings view: Profile (email, editable display name),
// Security (session rows), and Appearance (theme select, reduced-motion toggle, density).
import { describe, it, expect, vi, beforeEach } from 'vitest'
import { fireEvent, waitFor, screen } from '@testing-library/react'
import { renderAt } from '../test/utils'
import type { Capabilities } from '../api'

const caps: Capabilities = {
  edition: 'community',
  billing: false,
  signup: false,
  teams: false,
  idp: 'oidc',
  orgSwitcher: false,
  secrets: { providers: ['kube'] },
  proof: false,
  ownership: 'self-hosted',
}

const accountPayload = {
  account_id: 'acc-1',
  email: 'alice@example.com',
  display_name: 'Alice',
  timezone: 'UTC',
  locale: 'en',
  memberships: [{ account_id: 'acc-1', org_id: 'org-1', role: 'owner', created_at: '2026-01-01T00:00:00Z' }],
}

const sessionsPayload = {
  sessions: [
    { id: 's1', label: 'Chrome on Linux', created_at: '2026-06-01T00:00:00Z', current: true },
    { id: 's2', label: 'Firefox on macOS', created_at: '2026-06-10T00:00:00Z', current: false },
  ],
}

function mockFetch() {
  vi.spyOn(globalThis, 'fetch').mockImplementation((input, init) => {
    const url = String(input).split('?')[0]
    const method = (init?.method ?? 'GET').toUpperCase()

    if (url.endsWith('/console/capabilities')) {
      return Promise.resolve(new Response(JSON.stringify(caps), { status: 200, headers: { 'content-type': 'application/json' } }))
    }
    if (url.endsWith('/console/account') && method === 'GET') {
      return Promise.resolve(new Response(JSON.stringify(accountPayload), { status: 200, headers: { 'content-type': 'application/json' } }))
    }
    if (url.endsWith('/console/account') && method === 'PATCH') {
      return Promise.resolve(new Response(JSON.stringify(accountPayload), { status: 200, headers: { 'content-type': 'application/json' } }))
    }
    if (url.endsWith('/console/account/sessions') && method === 'GET') {
      return Promise.resolve(new Response(JSON.stringify(sessionsPayload), { status: 200, headers: { 'content-type': 'application/json' } }))
    }
    if (url.match(/\/console\/account\/sessions\/[^/]+/) && method === 'DELETE') {
      return Promise.resolve(new Response(null, { status: 204 }))
    }
    if (url.endsWith('/console/account/sessions') && method === 'DELETE') {
      return Promise.resolve(new Response(null, { status: 204 }))
    }
    return Promise.resolve(new Response(JSON.stringify({}), { status: 200, headers: { 'content-type': 'application/json' } }))
  })
}

beforeEach(() => {
  mockFetch()
  localStorage.clear()
  delete document.documentElement.dataset['reduceMotion']
  delete document.documentElement.dataset['density']
  delete document.documentElement.dataset['theme']
})

describe('Settings view', () => {
  it('renders the Profile tab with read-only email and editable display-name input', async () => {
    await renderAt('/settings', caps)
    await waitFor(() => expect(screen.getByText('alice@example.com')).toBeInTheDocument())
    expect(screen.getByLabelText(/display.?name/i)).toBeInTheDocument()
    const input = screen.getByLabelText(/display.?name/i)
    expect(input.tagName).toBe('INPUT')
  })

  it('shows membership rows with role badges in the Profile tab', async () => {
    await renderAt('/settings', caps)
    await waitFor(() => expect(screen.getByText('alice@example.com')).toBeInTheDocument())
    expect(screen.getByText('org-1')).toBeInTheDocument()
    // Role badge is present
    const badge = document.querySelector('.role-badge')
    expect(badge).not.toBeNull()
  })

  it('switches to Security and shows session rows with Revoke buttons', async () => {
    await renderAt('/settings', caps)
    // Switch to Security tab
    const securityTab = await waitFor(() => screen.getByRole('tab', { name: /security/i }))
    fireEvent.click(securityTab)
    await waitFor(() => expect(screen.getByText('Chrome on Linux')).toBeInTheDocument())
    expect(screen.getByText('Firefox on macOS')).toBeInTheDocument()
    const revokeButtons = screen.getAllByRole('button', { name: /revoke/i })
    expect(revokeButtons.length).toBeGreaterThanOrEqual(1)
  })

  it('shows a Sign out everywhere button in the Security tab', async () => {
    await renderAt('/settings', caps)
    const securityTab = await waitFor(() => screen.getByRole('tab', { name: /security/i }))
    fireEvent.click(securityTab)
    await waitFor(() => expect(screen.getByRole('button', { name: /sign out everywhere/i })).toBeInTheDocument())
  })

  it('switches to Appearance and toggling reduced-motion sets dataset.reduceMotion', async () => {
    await renderAt('/settings', caps)
    const appearanceTab = await waitFor(() => screen.getByRole('tab', { name: /appearance/i }))
    fireEvent.click(appearanceTab)
    const checkbox = await waitFor(() => screen.getByRole('checkbox', { name: /reduced.?motion/i }))
    expect(checkbox).toBeInTheDocument()
    // Toggle it on
    fireEvent.click(checkbox)
    expect(document.documentElement.dataset['reduceMotion']).toBe('1')
  })

  it('density select in Appearance applies dataset.density immediately', async () => {
    await renderAt('/settings', caps)
    const appearanceTab = await waitFor(() => screen.getByRole('tab', { name: /appearance/i }))
    fireEvent.click(appearanceTab)
    const densitySelect = await waitFor(() => screen.getByRole('combobox', { name: /density/i }))
    fireEvent.change(densitySelect, { target: { value: 'compact' } })
    expect(document.documentElement.dataset['density']).toBe('compact')
  })

  it('theme select offers System, Dark, and Light and applies dataset.theme immediately', async () => {
    await renderAt('/settings', caps)
    const appearanceTab = await waitFor(() => screen.getByRole('tab', { name: /appearance/i }))
    fireEvent.click(appearanceTab)
    const themeSelect = await waitFor(() => screen.getByRole('combobox', { name: /theme/i }))
    const labels = Array.from((themeSelect as HTMLSelectElement).options).map((o) => o.textContent)
    expect(labels).toEqual(['System', 'Dark', 'Light'])

    fireEvent.change(themeSelect, { target: { value: 'light' } })
    expect(document.documentElement.dataset['theme']).toBe('light')

    fireEvent.change(themeSelect, { target: { value: 'dark' } })
    expect(document.documentElement.dataset['theme']).toBe('dark')

    // System removes the attribute so the prefers-color-scheme default decides.
    fireEvent.change(themeSelect, { target: { value: 'system' } })
    expect(document.documentElement.dataset['theme']).toBeUndefined()
  })

  it('persists the chosen theme to localStorage', async () => {
    await renderAt('/settings', caps)
    const appearanceTab = await waitFor(() => screen.getByRole('tab', { name: /appearance/i }))
    fireEvent.click(appearanceTab)
    const themeSelect = await waitFor(() => screen.getByRole('combobox', { name: /theme/i }))
    fireEvent.change(themeSelect, { target: { value: 'light' } })
    const stored = JSON.parse(localStorage.getItem('mitos-appearance') ?? '{}')
    expect(stored.theme).toBe('light')
  })
})
