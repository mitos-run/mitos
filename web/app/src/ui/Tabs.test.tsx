import { describe, it, expect, vi } from 'vitest'
import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { Tabs } from './Tabs'

describe('Tabs', () => {
  it('renders a tablist and fires onChange on click and arrow keys', async () => {
    const onChange = vi.fn()
    render(<Tabs tabs={[{ key: 'a', label: 'Overview' }, { key: 'b', label: 'Logs' }]} active="a" onChange={onChange} />)
    const list = screen.getByRole('tablist')
    expect(list).toBeInTheDocument()
    const overview = screen.getByRole('tab', { name: 'Overview' })
    expect(overview).toHaveAttribute('aria-selected', 'true')
    await userEvent.click(screen.getByRole('tab', { name: 'Logs' }))
    expect(onChange).toHaveBeenCalledWith('b')
    overview.focus()
    await userEvent.keyboard('{ArrowRight}')
    expect(onChange).toHaveBeenCalledWith('b')
  })
})
