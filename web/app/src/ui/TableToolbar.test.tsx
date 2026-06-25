import { describe, it, expect } from 'vitest'
import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { renderHook, act } from '@testing-library/react'
import { TableToolbar, useTableFilter } from './TableToolbar'

describe('useTableFilter', () => {
  it('filters rows by the query across the provided text, case-insensitively', () => {
    const rows = [{ id: 'alice' }, { id: 'bob' }, { id: 'carol' }]
    const { result } = renderHook(() => useTableFilter(rows, (r) => r.id))
    expect(result.current.filtered).toHaveLength(3)
    act(() => result.current.setQuery('BO'))
    expect(result.current.filtered.map((r) => r.id)).toEqual(['bob'])
  })

  it('returns all rows when the query is blank', () => {
    const rows = [{ id: 'a' }, { id: 'b' }]
    const { result } = renderHook(() => useTableFilter(rows, (r) => r.id))
    act(() => result.current.setQuery('   '))
    expect(result.current.filtered).toHaveLength(2)
  })
})

describe('TableToolbar', () => {
  it('renders a labelled search box and the count, and reports changes', async () => {
    const onQueryChange = (v: string) => calls.push(v)
    const calls: string[] = []
    render(<TableToolbar query="" onQueryChange={onQueryChange} count={5} noun="members" />)
    expect(screen.getByText('5 members')).toBeInTheDocument()
    const input = screen.getByRole('searchbox', { name: /search members/i })
    await userEvent.type(input, 'x')
    expect(calls).toContain('x')
  })
})
