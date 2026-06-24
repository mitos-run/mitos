import { describe, it, expect, vi, beforeEach } from 'vitest'
import { waitFor, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { renderAt } from '../test/utils'
import type { Capabilities } from '../api'

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

describe('AppShell', () => {
  it('renders the group headers and the visible nav links', async () => {
    await renderAt('/sandboxes', caps)
    await waitFor(() => expect(screen.getByRole('link', { name: 'Sandboxes' })).toBeInTheDocument())
    expect(screen.getByText('Run')).toBeInTheDocument()
    expect(screen.getByText('Govern')).toBeInTheDocument()
    // billing is off -> no Billing link
    expect(screen.queryByRole('link', { name: 'Billing' })).not.toBeInTheDocument()
  })

  it('shows the self-hosted ownership badge', async () => {
    await renderAt('/sandboxes', caps)
    await waitFor(() => expect(screen.getByText('Self-hosted')).toBeInTheDocument())
  })
})

describe('AppShell responsive drawer', () => {
  it('exposes a menu button that toggles the nav drawer and reports aria-expanded', async () => {
    const user = userEvent.setup()
    await renderAt('/sandboxes', caps)
    const menu = await screen.findByRole('button', { name: /menu/i })
    expect(menu).toHaveAttribute('aria-expanded', 'false')
    await user.click(menu)
    expect(menu).toHaveAttribute('aria-expanded', 'true')
  })

  it('closes the drawer on Escape', async () => {
    const user = userEvent.setup()
    await renderAt('/sandboxes', caps)
    const menu = await screen.findByRole('button', { name: /menu/i })
    await user.click(menu)
    expect(menu).toHaveAttribute('aria-expanded', 'true')
    await user.keyboard('{Escape}')
    expect(menu).toHaveAttribute('aria-expanded', 'false')
  })

  it('gives the primary nav an accessible name', async () => {
    await renderAt('/sandboxes', caps)
    expect(await screen.findByRole('navigation', { name: /primary/i })).toBeInTheDocument()
  })

  it('moves focus into the primary nav when the drawer opens', async () => {
    const user = userEvent.setup()
    await renderAt('/sandboxes', caps)
    const menu = await screen.findByRole('button', { name: /menu/i })
    await user.click(menu)
    const nav = screen.getByRole('navigation', { name: /primary/i })
    expect(nav).toHaveFocus()
  })

  it('returns focus to the menu button when the drawer closes via Escape', async () => {
    const user = userEvent.setup()
    await renderAt('/sandboxes', caps)
    const menu = await screen.findByRole('button', { name: /menu/i })
    await user.click(menu)
    await user.keyboard('{Escape}')
    expect(menu).toHaveFocus()
  })
})
