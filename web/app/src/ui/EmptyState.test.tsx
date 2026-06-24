import { describe, it, expect, vi } from 'vitest'
import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { EmptyState } from './EmptyState'

describe('EmptyState', () => {
  it('renders title and body and fires the action', async () => {
    const onClick = vi.fn()
    render(<EmptyState title="No sandboxes yet" body="Fork your first one." action={{ label: 'Fork', onClick }} />)
    expect(screen.getByText('No sandboxes yet')).toBeInTheDocument()
    expect(screen.getByText('Fork your first one.')).toBeInTheDocument()
    await userEvent.click(screen.getByRole('button', { name: 'Fork' }))
    expect(onClick).toHaveBeenCalledOnce()
  })
})
