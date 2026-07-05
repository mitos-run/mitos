import { describe, it, expect, vi, beforeEach } from 'vitest'
import { waitFor, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { renderAt } from '../test/utils'
import type { Capabilities } from '../api'

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

  // Regression guard for a real mobile-viewport bug (found via Playwright at
  // 375px, not visible in jsdom): <main> is a flex item (flex:1 inside
  // .app-shell's display:flex) with no explicit min-width. A flex item's
  // default min-width is 'auto', which lets its own automatic minimum size
  // grow to fit the min-content size of ANY descendant, even one many levels
  // deep with its own overflow-x:auto wrapper (e.g. a .tbl forced to
  // min-width:600px on a narrow viewport). The wrapper's overflow-x:auto only
  // clips ITS children; it does nothing to stop this flex item from being
  // sized to fit them, so without min-width:0 here a single wide table
  // anywhere on the page silently made the whole page body scroll
  // horizontally on a phone. jsdom has no layout engine so this cannot be
  // asserted via getBoundingClientRect; asserting the inline style directly
  // pins the fix that a future refactor could otherwise drop unnoticed.
  it('gives the routed content pane min-width:0 so a wide descendant cannot force page-body horizontal scroll', async () => {
    await renderAt('/sandboxes', caps)
    await waitFor(() => expect(screen.getByRole('link', { name: 'Sandboxes' })).toBeInTheDocument())
    const main = document.querySelector('main')
    expect(main).not.toBeNull()
    expect((main as HTMLElement).style.minWidth).toBe('0')
  })

  it('shows the version footer under the ownership badge and copies it (with edition) on click', async () => {
    const withVersion: Capabilities = { ...caps, version: '1.6.0' }
    vi.spyOn(globalThis, 'fetch').mockImplementation((input) => {
      const url = String(input)
      if (url.endsWith('/console/capabilities')) {
        return Promise.resolve(new Response(JSON.stringify(withVersion), { status: 200, headers: { 'content-type': 'application/json' } }))
      }
      return Promise.resolve(new Response(JSON.stringify({ sandboxes: [], secrets: [] }), { status: 200, headers: { 'content-type': 'application/json' } }))
    })
    const writeText = vi.fn().mockResolvedValue(undefined)
    Object.assign(navigator, { clipboard: { writeText } })

    await renderAt('/sandboxes', withVersion)
    const versionButton = await screen.findByRole('button', { name: /copy version.*mitos 1\.6\.0/i })
    expect(versionButton).toHaveTextContent('mitos 1.6.0')
    await userEvent.click(versionButton)
    expect(writeText).toHaveBeenCalledWith('mitos 1.6.0 (community)')
    expect(await screen.findByText('Copied')).toBeInTheDocument()
  })

  it('renders no version footer when the server does not advertise a version (older server)', async () => {
    await renderAt('/sandboxes', caps)
    await waitFor(() => expect(screen.getByRole('link', { name: 'Sandboxes' })).toBeInTheDocument())
    expect(screen.queryByText(/^mitos /)).not.toBeInTheDocument()
  })

  it('shows no ownership badge on the hosted edition', async () => {
    const hosted: Capabilities = { ...caps, ownership: 'hosted' }
    vi.spyOn(globalThis, 'fetch').mockImplementation((input) => {
      const url = String(input)
      if (url.endsWith('/console/capabilities')) {
        return Promise.resolve(new Response(JSON.stringify(hosted), { status: 200, headers: { 'content-type': 'application/json' } }))
      }
      return Promise.resolve(new Response(JSON.stringify({ sandboxes: [], secrets: [] }), { status: 200, headers: { 'content-type': 'application/json' } }))
    })
    await renderAt('/sandboxes', hosted)
    await waitFor(() => expect(screen.getByRole('link', { name: 'Sandboxes' })).toBeInTheDocument())
    expect(screen.queryByText('Self-hosted')).not.toBeInTheDocument()
    expect(screen.queryByText(/Hosted by Mitos/)).not.toBeInTheDocument()
  })
})

describe('AppShell responsive drawer', () => {
  it('exposes a menu button that toggles the nav drawer and reports aria-expanded', async () => {
    const user = userEvent.setup()
    await renderAt('/sandboxes', caps)
    const menu = await screen.findByRole('button', { name: /open navigation menu/i })
    expect(menu).toHaveAttribute('aria-expanded', 'false')
    await user.click(menu)
    expect(menu).toHaveAttribute('aria-expanded', 'true')
  })

  it('closes the drawer on Escape', async () => {
    const user = userEvent.setup()
    await renderAt('/sandboxes', caps)
    const menu = await screen.findByRole('button', { name: /open navigation menu/i })
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
    const menu = await screen.findByRole('button', { name: /open navigation menu/i })
    await user.click(menu)
    const nav = screen.getByRole('navigation', { name: /primary/i })
    expect(nav).toHaveFocus()
  })

  it('returns focus to the menu button when the drawer closes via Escape', async () => {
    const user = userEvent.setup()
    await renderAt('/sandboxes', caps)
    const menu = await screen.findByRole('button', { name: /open navigation menu/i })
    await user.click(menu)
    await user.keyboard('{Escape}')
    expect(menu).toHaveFocus()
  })
})
