import { describe, it, expect, vi } from 'vitest'
import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { createRef } from 'react'
import { TopBar } from './TopBar'

vi.mock('@tanstack/react-router', () => ({ Link: (p: any) => <a href={p.to}>{p.children}</a> }))
vi.mock('../data/account-settings', () => ({
  useAccount: () => ({ data: { display_name: 'Alice', email: 'alice@acme.dev', memberships: [] } }),
  useSignOut: () => ({ mutate: vi.fn(), isPending: false }),
}))

describe('TopBar', () => {
  it('renders the brand, a search trigger that fires onSearch, and the account menu', async () => {
    const onSearch = vi.fn()
    render(<TopBar onSearch={onSearch} onToggleDrawer={() => {}} drawerOpen={false} menuButtonRef={createRef()} />)
    expect(screen.getByText('Mitos')).toBeInTheDocument()
    await userEvent.click(screen.getByRole('button', { name: /search/i }))
    expect(onSearch).toHaveBeenCalled()
    expect(screen.getByRole('button', { name: /account menu/i })).toBeInTheDocument()
  })
})
