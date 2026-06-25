import { describe, it, expect, vi } from 'vitest'
import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { SearchTrigger } from './SearchTrigger'

describe('SearchTrigger', () => {
  it('renders a labelled button and calls onClick', async () => {
    const onClick = vi.fn()
    render(<SearchTrigger onClick={onClick} />)
    const btn = screen.getByRole('button', { name: /search/i })
    await userEvent.click(btn)
    expect(onClick).toHaveBeenCalledTimes(1)
  })
})
