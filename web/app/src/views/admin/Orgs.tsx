// Instance-operator org table: every org's plan tier, member count, running
// sandboxes, and month-to-date spend, so an operator can spot an outlier org
// without leaving this page. The server caps the per-org rollup at 200
// (oldest orgs first) on a large deployment; Total is always the TRUE org
// count, so a capped result says so honestly rather than looking complete.
import { useAdminOrgs } from '../../data/admin'
import { Skeleton } from '../../ui/Skeleton'
import { EmptyState } from '../../ui/EmptyState'
import { PageHeader } from '../../ui/PageHeader'
import { TableToolbar, useTableFilter } from '../../ui/TableToolbar'
import { fmtDollars } from '../../api'

export function AdminOrgs() {
  const { data, isLoading, isError } = useAdminOrgs()
  const orgs = data?.orgs ?? []
  const { query, setQuery, filtered } = useTableFilter(orgs, (o) => `${o.id} ${o.name} ${o.tier}`)

  return (
    <section>
      <PageHeader
        title="Organizations"
        eyebrow="Operate"
        lede="Every organization on this deployment, with plan tier, membership, live sandboxes, and month-to-date usage."
      />
      {isLoading ? (
        <Skeleton rows={4} />
      ) : isError ? (
        <p className="t-dim">Failed to load organizations. Please refresh.</p>
      ) : orgs.length === 0 ? (
        <EmptyState title="No organizations" body="No organization has been created on this deployment yet." />
      ) : (
        <div style={{ overflowX: 'auto' }}>
          <TableToolbar query={query} onQueryChange={setQuery} count={filtered.length} noun="organizations" />
          {data && data.total > orgs.length && (
            <p className="t-dim" style={{ fontSize: 'var(--step--1)', margin: '0 0 var(--space-3)' }}>
              Showing {orgs.length} of {data.total} organizations (oldest first; the rollup is capped on large deployments).
            </p>
          )}
          {!!data?.failed_orgs && (
            <p className="t-dim" style={{ fontSize: 'var(--step--1)', margin: '0 0 var(--space-3)' }}>
              {data.failed_orgs} organization{data.failed_orgs === 1 ? '' : 's'} could not be read and{' '}
              {data.failed_orgs === 1 ? 'is' : 'are'} omitted from these figures.
            </p>
          )}
          <table className="tbl" aria-label="Organizations">
            <thead>
              <tr>
                <th scope="col">Organization</th>
                <th scope="col">Tier</th>
                <th scope="col">Members</th>
                <th scope="col">Running</th>
                <th scope="col">Month-to-date</th>
              </tr>
            </thead>
            <tbody>
              {filtered.map((o) => (
                <tr key={o.id}>
                  <td>
                    <div>{o.name}</div>
                    <div className="t-dim mono" style={{ fontSize: 'var(--step--2)' }}>{o.id}</div>
                  </td>
                  <td>
                    <span
                      style={{
                        display: 'inline-block',
                        padding: 'var(--space-1) var(--space-2)',
                        borderRadius: 'var(--r-sm)',
                        fontSize: 'var(--step--1)',
                        border: '1px solid var(--hairline)',
                        color: 'var(--ink-2)',
                      }}
                    >
                      {o.tier}
                    </span>
                  </td>
                  <td>{o.members}</td>
                  <td>{o.running}</td>
                  <td className="mono">{fmtDollars(o.month_usage_cents)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </section>
  )
}
