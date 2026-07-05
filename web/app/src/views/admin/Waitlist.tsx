// Instance-operator waitlist: signups recorded while this deployment is in
// waitlist mode. Approve grants the email allowlist access and sends the
// "you're in" notification through the funnel's configured email sender; it
// does NOT create an account or org itself, so an approved entry still shows
// here until the person completes signup (there is no separate "approved"
// state to move it to yet, a documented follow-up).
import { useState } from 'react'
import { useAdminWaitlist, useApproveWaitlistEntry } from '../../data/admin'
import { useAccount } from '../../data/account-settings'
import { Skeleton } from '../../ui/Skeleton'
import { EmptyState } from '../../ui/EmptyState'
import { useToast } from '../../ui/Toast'
import { PageHeader } from '../../ui/PageHeader'
import { TableToolbar, useTableFilter } from '../../ui/TableToolbar'
import { fmtAbsolute } from '../../lib/dates'

export function AdminWaitlist() {
  const { data: entries = [], isLoading, isError } = useAdminWaitlist()
  const { data: account } = useAccount()
  const approve = useApproveWaitlistEntry()
  const { notify } = useToast()
  const { query, setQuery, filtered } = useTableFilter(entries, (e) => e.email)
  const [pendingID, setPendingID] = useState<string | null>(null)

  function onApprove(id: string, email: string) {
    setPendingID(id)
    approve.mutate(id, {
      onSuccess: () => notify(`Approved ${email}`, 'ok'),
      onError: () => notify(`Failed to approve ${email}`, 'error'),
      onSettled: () => setPendingID(null),
    })
  }

  return (
    <section>
      <PageHeader
        title="Waitlist"
        eyebrow="Operate"
        lede="Signups recorded while this deployment is in waitlist mode. Approving grants allowlist access and emails the person."
      />
      {isLoading ? (
        <Skeleton rows={4} />
      ) : isError ? (
        <p className="t-dim">Failed to load the waitlist. Please refresh.</p>
      ) : entries.length === 0 ? (
        <EmptyState title="No waitlist entries" body="Nobody is currently waiting for access on this deployment." />
      ) : (
        <div style={{ overflowX: 'auto' }}>
          <TableToolbar query={query} onQueryChange={setQuery} count={filtered.length} noun="entries" />
          <table className="tbl" aria-label="Waitlist">
            <thead>
              <tr>
                <th scope="col">Email</th>
                <th scope="col">Recorded</th>
                <th scope="col"><span className="sr-only">Actions</span></th>
              </tr>
            </thead>
            <tbody>
              {filtered.map((e) => (
                <tr key={e.id}>
                  <td>{e.email}</td>
                  <td className="t-dim">{fmtAbsolute(e.created_at, account?.locale, account?.timezone)}</td>
                  <td>
                    <button
                      className="btn"
                      onClick={() => onApprove(e.id, e.email)}
                      disabled={pendingID === e.id}
                      aria-label={`Approve ${e.email}`}
                    >
                      {pendingID === e.id ? 'Approving...' : 'Approve'}
                    </button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </section>
  )
}
