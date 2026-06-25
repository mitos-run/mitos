// The table toolbar: a client-side search box and a live result count above a
// list view. Filtering happens over rows already fetched (no extra requests),
// which is the power-user affordance the list views were missing. Pair the
// toolbar with useTableFilter, which owns the query and the filtered rows.
import { useMemo, useState } from 'react'
import type { ReactNode } from 'react'

export function useTableFilter<T>(rows: T[], toText: (row: T) => string): {
  query: string
  setQuery: (q: string) => void
  filtered: T[]
} {
  const [query, setQuery] = useState('')
  const q = query.trim().toLowerCase()
  const filtered = useMemo(
    () => (q ? rows.filter((r) => toText(r).toLowerCase().includes(q)) : rows),
    // toText is assumed pure; recompute when the rows or the query change.
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [rows, q],
  )
  return { query, setQuery, filtered }
}

export function TableToolbar({ query, onQueryChange, count, noun, children }: {
  query: string
  onQueryChange: (q: string) => void
  count: number
  noun: string
  children?: ReactNode
}) {
  return (
    <div className="table-toolbar">
      <input
        className="table-search"
        type="search"
        placeholder={`Search ${noun}...`}
        aria-label={`Search ${noun}`}
        value={query}
        onChange={(e) => onQueryChange(e.target.value)}
      />
      {children}
      <span className="table-count t-dim">{count} {noun}</span>
    </div>
  )
}
