import { describe, it, expect, vi } from 'vitest'
import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { createRef } from 'react'
import { axe } from 'vitest-axe'
import * as matchers from 'vitest-axe/matchers'
import { TopBar } from './TopBar'

expect.extend(matchers)
vi.mock('@tanstack/react-router', () => ({ Link: (p: any) => <a href={p.to} role={p.role} onClick={p.onClick}>{p.children}</a> }))
vi.mock('../data/account-settings', () => ({
  useAccount: () => ({ data: { display_name: 'Alice', email: 'alice@acme.dev', memberships: [] } }),
  useSignOut: () => ({ mutate: vi.fn(), isPending: false }),
}))

describe('TopBar a11y', () => {
  it('has no axe violations with the account menu open', async () => {
    const { container } = render(<TopBar onSearch={() => {}} onToggleDrawer={() => {}} drawerOpen={false} menuButtonRef={createRef()} />)
    await userEvent.click(screen.getByRole('button', { name: /account menu/i }))
    expect(await axe(container)).toHaveNoViolations()
  })
})
