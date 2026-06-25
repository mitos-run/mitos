import { describe, it, expect, vi } from 'vitest'
import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { CommandPalette, fuzzyMatch } from './CommandPalette'
import type { Capabilities } from '../api'

const caps: Capabilities = {
  edition: 'community', billing: false, signup: false, teams: true, idp: 'oidc',
  orgSwitcher: false, secrets: { providers: ['kube'] }, proof: true, ownership: 'self-hosted',
}

vi.mock('@tanstack/react-router', () => ({ useNavigate: () => () => {} }))

describe('CommandPalette (controlled)', () => {
  it('renders nothing when open is false', () => {
    const { container } = render(<CommandPalette caps={caps} open={false} onOpenChange={() => {}} />)
    expect(container.firstChild).toBeNull()
  })

  it('renders the input when open is true and reports close on Escape', async () => {
    const onOpenChange = vi.fn()
    render(<CommandPalette caps={caps} open onOpenChange={onOpenChange} />)
    const input = screen.getByLabelText('Command palette input')
    expect(input).toBeInTheDocument()
    await userEvent.type(input, '{Escape}')
    expect(onOpenChange).toHaveBeenCalledWith(false)
  })

  it('fuzzyMatch still matches subsequences', () => {
    expect(fuzzyMatch('ovw', 'Overview')).toBe(true)
    expect(fuzzyMatch('zzz', 'Overview')).toBe(false)
  })
})
