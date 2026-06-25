// Audit view: client-side filterable event table. Columns: Time, Actor, Action,
// Target, Detail. Filter input covers actor_id, action, target, and detail
// fields (case-insensitive). Consumes the live BFF via useAudit().
import { useState } from 'react'
import { useAudit } from '../data/account'
import { Skeleton } from '../ui/Skeleton'
import { EmptyState } from '../ui/EmptyState'

export function Audit() {
  const { data: events = [], isLoading } = useAudit()
  const [filter, setFilter] = useState('')

  const filtered = filter
    ? events.filter((e) => {
        const q = filter.toLowerCase()
        return (
          e.actor_id.toLowerCase().includes(q) ||
          e.action.toLowerCase().includes(q) ||
          e.target.toLowerCase().includes(q) ||
          e.detail.toLowerCase().includes(q)
        )
      })
    : events

  return (
    <section>
      <h2>Audit log</h2>
      <p className="t-dim" style={{ fontSize: 'var(--step--1)', marginBottom: 'var(--space-5)' }}>
        Immutable record of org-scoped actions. Filter by actor, action, target, or detail.
      </p>

      {isLoading ? (
        <Skeleton rows={5} />
      ) : (
        <>
          <div style={{ marginBottom: 'var(--space-4)' }}>
            <input
              aria-label="Filter audit"
              placeholder="Filter by actor, action, target, or detail"
              value={filter}
              onChange={(e) => setFilter(e.target.value)}
              style={{ width: '100%', maxWidth: '400px' }}
            />
          </div>

          {filtered.length === 0 ? (
            <EmptyState
              title={events.length === 0 ? 'No audit events yet' : 'No matching events'}
              body={events.length === 0 ? 'Actions taken in this org will appear here.' : 'Try a different filter term.'}
            />
          ) : (
            <div style={{ overflowX: 'auto' }}>
              <table className="tbl" aria-label="Audit log">
                <thead>
                  <tr>
                    <th scope="col">Time</th>
                    <th scope="col">Actor</th>
                    <th scope="col">Action</th>
                    <th scope="col">Target</th>
                    <th scope="col">Detail</th>
                  </tr>
                </thead>
                <tbody>
                  {filtered.map((e, i) => (
                    <tr key={i}>
                      <td className="t-dim">{new Date(e.at).toLocaleString()}</td>
                      <td className="mono">{e.actor_id}</td>
                      <td className="mono">{e.action}</td>
                      <td className="mono">{e.target}</td>
                      <td>{e.detail}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </>
      )}
    </section>
  )
}
