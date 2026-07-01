import { describe, it, expect, vi } from 'vitest'
import { render, screen } from '@testing-library/react'
import { Celebrate } from './Celebrate'

// jsdom does not implement window.matchMedia; stub it per test so the component
// can read prefers-reduced-motion without throwing.
function mockMatchMedia(matches: boolean) {
  Object.defineProperty(window, 'matchMedia', {
    writable: true,
    configurable: true,
    value: vi.fn().mockReturnValue({ matches }),
  })
}

describe('Celebrate', () => {
  it('renders status and burst when active and motion allowed', () => {
    mockMatchMedia(false)
    render(<Celebrate active={true} />)
    const status = screen.getByRole('status')
    expect(status).toHaveTextContent('You are live')
    expect(screen.queryByTestId('confetti-burst')).toBeInTheDocument()
  })

  it('renders only status under prefers-reduced-motion', () => {
    mockMatchMedia(true)
    render(<Celebrate active={true} />)
    expect(screen.getByRole('status')).toHaveTextContent('You are live')
    expect(screen.queryByTestId('confetti-burst')).not.toBeInTheDocument()
  })

  it('renders nothing when not active', () => {
    mockMatchMedia(false)
    const { container } = render(<Celebrate active={false} />)
    expect(screen.queryByRole('status')).not.toBeInTheDocument()
    expect(screen.queryByTestId('confetti-burst')).not.toBeInTheDocument()
    expect(container.firstChild).toBeNull()
  })
})
