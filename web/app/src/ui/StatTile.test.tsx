import { describe, it, expect } from 'vitest'
import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { StatTile } from './StatTile'

describe('StatTile', () => {
  it('renders label, value, and unit', () => {
    render(<StatTile label="Activate P50" value="27" unit="ms" />)
    expect(screen.getByText('Activate P50')).toBeInTheDocument()
    expect(screen.getByText('27')).toBeInTheDocument()
    expect(screen.getByText('ms')).toBeInTheDocument()
  })

  it('discloses the reproduce command on demand', async () => {
    render(<StatTile label="Activate P50" value="27" unit="ms" reproduce={{ label: 'Reproduce this', command: 'bench/husk-activate-latency.sh' }} />)
    const btn = screen.getByRole('button', { name: /reproduce this/i })
    expect(screen.queryByText('bench/husk-activate-latency.sh')).not.toBeInTheDocument()
    await userEvent.click(btn)
    expect(screen.getByText('bench/husk-activate-latency.sh')).toBeInTheDocument()
    expect(btn).toHaveAttribute('aria-expanded', 'true')
  })
})
