import { describe, it, expect, vi } from 'vitest'
import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { AccountMenu } from './AccountMenu'

vi.mock('@tanstack/react-router', () => ({ Link: (p: any) => <a href={p.to}>{p.children}</a> }))
vi.mock('../data/account-settings', () => ({
  useAccount: () => ({ data: { account_id: 'a1', email: 'alice@acme.dev', display_name: 'Alice Anderson', timezone: 'UTC', locale: 'en', memberships: [] } }),
  useSignOut: () => ({ mutate: vi.fn(), isPending: false }),
}))

describe('AccountMenu', () => {
  it('opens to show identity and actions, closes on Escape', async () => {
    render(<AccountMenu />)
    const btn = screen.getByRole('button', { name: /account menu/i })
    expect(btn).toHaveAttribute('aria-expanded', 'false')
    await userEvent.click(btn)
    expect(btn).toHaveAttribute('aria-expanded', 'true')
    expect(screen.getByText('alice@acme.dev')).toBeInTheDocument()
    expect(screen.getByText(/account settings/i)).toBeInTheDocument()
    expect(screen.getByText(/sign out/i)).toBeInTheDocument()
    await userEvent.keyboard('{Escape}')
    expect(btn).toHaveAttribute('aria-expanded', 'false')
  })
})
