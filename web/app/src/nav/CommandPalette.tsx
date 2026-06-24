// Cmd-K command palette: fuzzy navigation to any visible route. Opens on
// Cmd/Ctrl-K, filters as you type, navigates on Enter or click. Actions (fork a
// sandbox, create a key) are added by later phases as those flows land; this B0
// version is navigation-complete.
import { useEffect, useMemo, useState } from 'react'
import { useNavigate } from '@tanstack/react-router'
import { visibleRoutes } from './routes'
import type { Capabilities } from '../api'

// Subsequence match: every char of query appears in order in label. Cheap and
// good enough for a route list; case-insensitive.
export function fuzzyMatch(query: string, label: string): boolean {
  const q = query.toLowerCase()
  const l = label.toLowerCase()
  let i = 0
  for (const ch of l) {
    if (i < q.length && ch === q[i]) i++
  }
  return i === q.length
}

export function CommandPalette({ caps }: { caps: Capabilities }) {
  const [open, setOpen] = useState(false)
  const [query, setQuery] = useState('')
  const navigate = useNavigate()
  const routes = useMemo(() => visibleRoutes(caps), [caps])
  const results = useMemo(() => routes.filter((r) => fuzzyMatch(query, r.label)), [routes, query])

  useEffect(() => {
    function onKey(e: KeyboardEvent) {
      if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === 'k') {
        e.preventDefault()
        setOpen((v) => {
          if (!v) setQuery('')
          return !v
        })
      }
      if (e.key === 'Escape') setOpen(false)
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [])

  if (!open) return null

  function go(path: string) {
    setOpen(false)
    setQuery('')
    void navigate({ to: path })
  }

  return (
    <div role="dialog" aria-label="Command palette" className="palette-backdrop" onClick={() => setOpen(false)}>
      <div className="palette" onClick={(e) => e.stopPropagation()}>
        <input
          autoFocus
          aria-label="Command palette input"
          placeholder="Jump to..."
          value={query}
          onChange={(e) => setQuery(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === 'Enter' && results[0]) go(results[0].path)
          }}
        />
        <ul>
          {results.map((r) => (
            <li key={r.path}>
              <button onClick={() => go(r.path)}>{r.label}<span className="t-dim"> {r.group}</span></button>
            </li>
          ))}
          {results.length === 0 && <li className="t-dim" style={{ padding: 'var(--space-2)' }}>No matches</li>}
        </ul>
      </div>
    </div>
  )
}
