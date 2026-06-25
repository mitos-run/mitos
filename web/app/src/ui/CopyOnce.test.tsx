import { describe, it, expect, vi } from 'vitest'
import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { CopyOnce } from './CopyOnce'

describe('CopyOnce', () => {
  it('shows the value once and copies it', async () => {
    const writeText = vi.fn().mockResolvedValue(undefined)
    Object.assign(navigator, { clipboard: { writeText } })
    render(<CopyOnce value="mitos_live_secret123" label="API key" />)
    expect(screen.getByText('mitos_live_secret123')).toBeInTheDocument()
    expect(screen.getByText(/shown once/i)).toBeInTheDocument()
    await userEvent.click(screen.getByRole('button', { name: /copy/i }))
    expect(writeText).toHaveBeenCalledWith('mitos_live_secret123')
  })
})
