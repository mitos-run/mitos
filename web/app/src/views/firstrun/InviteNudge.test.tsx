// InviteNudge.test.tsx: one-time dismissable "Bring your team" card.
//
// TDD: asserts the gating (caps.teams + members.length === 1), the dismiss
// button's localStorage persistence, and that it never renders once there is
// more than one member.

import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'

// Mock the data hooks so tests are deterministic.
vi.mock('../../data/query', () => ({
  useCapabilities: vi.fn(),
}))
vi.mock('../../data/org', () => ({
  useMembers: vi.fn(),
  useCreateInvite: vi.fn(),
}))

import { useCapabilities } from '../../data/query'
import { useMembers, useCreateInvite } from '../../data/org'
import { InviteNudge } from './InviteNudge'

const mockUseCapabilities = useCapabilities as ReturnType<typeof vi.fn>
const mockUseMembers = useMembers as ReturnType<typeof vi.fn>
const mockUseCreateInvite = useCreateInvite as ReturnType<typeof vi.fn>

const oneMember = [{ account_id: 'a1', role: 'owner', joined_at: '2026-01-01T00:00:00Z' }]
const twoMembers = [
  { account_id: 'a1', role: 'owner', joined_at: '2026-01-01T00:00:00Z' },
  { account_id: 'a2', role: 'member', joined_at: '2026-01-02T00:00:00Z' },
]

beforeEach(() => {
  vi.clearAllMocks()
  localStorage.clear()
  mockUseCapabilities.mockReturnValue({ data: { teams: true } })
  mockUseMembers.mockReturnValue({ data: oneMember, isLoading: false })
  mockUseCreateInvite.mockReturnValue({ mutateAsync: vi.fn().mockResolvedValue(undefined) })
})

describe('InviteNudge', () => {
  it('renders when caps.teams is on and there is exactly one member', () => {
    render(<InviteNudge />)
    expect(screen.getByText('Bring your team')).toBeInTheDocument()
  })

  it('does not render when caps.teams is off', () => {
    mockUseCapabilities.mockReturnValue({ data: { teams: false } })
    render(<InviteNudge />)
    expect(screen.queryByText('Bring your team')).not.toBeInTheDocument()
  })

  it('does not render when there is more than one member', () => {
    mockUseMembers.mockReturnValue({ data: twoMembers, isLoading: false })
    render(<InviteNudge />)
    expect(screen.queryByText('Bring your team')).not.toBeInTheDocument()
  })

  it('does not render while members are still loading (data undefined)', () => {
    mockUseMembers.mockReturnValue({ data: undefined, isLoading: true })
    render(<InviteNudge />)
    expect(screen.queryByText('Bring your team')).not.toBeInTheDocument()
  })

  it('opens the InviteModal when "Invite people" is clicked', async () => {
    render(<InviteNudge />)
    await userEvent.click(screen.getByRole('button', { name: /invite people/i }))
    expect(screen.getByRole('dialog', { name: /invite people/i })).toBeInTheDocument()
  })

  it('dismissing hides the card and persists to localStorage', async () => {
    render(<InviteNudge />)
    await userEvent.click(screen.getByRole('button', { name: /dismiss/i }))
    expect(screen.queryByText('Bring your team')).not.toBeInTheDocument()
    expect(localStorage.getItem('mitos-invite-nudge-dismissed')).toBe('1')
  })

  it('stays dismissed across remounts once persisted', () => {
    localStorage.setItem('mitos-invite-nudge-dismissed', '1')
    render(<InviteNudge />)
    expect(screen.queryByText('Bring your team')).not.toBeInTheDocument()
  })

  it('does not throw when localStorage is unavailable', () => {
    const spy = vi.spyOn(Storage.prototype, 'getItem').mockImplementation(() => {
      throw new Error('storage unavailable')
    })
    try {
      expect(() => render(<InviteNudge />)).not.toThrow()
      // Falls back to "not dismissed" so the nudge still shows.
      expect(screen.getByText('Bring your team')).toBeInTheDocument()
    } finally {
      spy.mockRestore()
    }
  })
})
