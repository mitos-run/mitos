// Instance-operator audit: the admin.* action namespace's own event stream
// (issue #714). Every /console/admin/... handler records one of these,
// including a DENIED authorizeAdmin attempt (action "admin.denied"), so
// this is the one place an operator sees them; a normal org's own audit
// view (views/Audit.tsx) never surfaces them, since they carry no OrgID any
// tenant-scoped view would match. Deliberately simple (no retention/export/
// sinks panel, unlike the org-scoped view): this is instance-level
// visibility, not a tenant-facing compliance surface.
import { useAdminAudit } from '../../data/admin'
import { useAccount } from '../../data/account-settings'
import { Skeleton } from '../../ui/Skeleton'
import { EmptyState } from '../../ui/EmptyState'
import { PageHeader } from '../../ui/PageHeader'
import { TableToolbar, useTableFilter } from '../../ui/TableToolbar'
import { fmtAbsolute } from '../../lib/dates'

export function AdminAudit() {
  const { data: events = [], isLoading, isError } = useAdminAudit()
  const { data: account } = useAccount()
  const { query, setQuery, filtered } = useTableFilter(
    events,
    (e) => `${e.actor_id} ${e.actor_name ?? ''} ${e.action} ${e.target} ${e.target_name ?? ''} ${e.detail}`,
  )

  return (
    <section>
      <PageHeader
        title="Audit"
        eyebrow="Operate"
        lede="This instance-operator plane's own events: every /console/admin/... read, waitlist approval, and denied access attempt."
      />
      {isLoading ? (
        <Skeleton rows={4} />
      ) : isError ? (
        <p className="t-dim">Failed to load the operator audit log. Please refresh.</p>
      ) : events.length === 0 ? (
        <EmptyState title="No operator activity yet" body="Actions taken on the instance-operator plane will appear here." />
      ) : (
        <div style={{ overflowX: 'auto' }}>
          <TableToolbar query={query} onQueryChange={setQuery} count={filtered.length} noun="events" />
          <table className="tbl" aria-label="Operator audit log">
            <thead>
              <tr>
                <th scope="col">Time</th>
                <th scope="col">Actor</th>
                <th scope="col">Action</th>
                <th scope="col">Detail</th>
              </tr>
            </thead>
            <tbody>
              {filtered.map((e, i) => (
                <tr key={i}>
                  <td className="t-dim">{fmtAbsolute(e.at, account?.locale, account?.timezone)}</td>
                  <td>
                    {e.actor_name ? (
                      <>
                        <div>{e.actor_name}</div>
                        <div className="mono t-dim" style={{ fontSize: 'var(--step--2)' }}>{e.actor_id || 'unauthenticated'}</div>
                      </>
                    ) : (
                      <span className="mono">{e.actor_id || 'unauthenticated'}</span>
                    )}
                  </td>
                  <td>
                    <span className="mono t-dim" style={{ fontSize: 'var(--step--2)' }}>{e.action}</span>
                  </td>
                  <td>{e.detail}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </section>
  )
}
