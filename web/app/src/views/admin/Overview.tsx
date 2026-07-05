// Instance-operator overview: the deployment-wide counts an operator lands
// on first. Node counts are shown only when the deployment has a configured
// NodeSource; otherwise an honest "not available" note, never a fabricated
// 0/0 (see the /admin/nodes view for the same rule at row granularity).
import { Link } from '@tanstack/react-router'
import { useAdminOverview } from '../../data/admin'
import { StatTile } from '../../ui/StatTile'
import { Skeleton } from '../../ui/Skeleton'
import { PageHeader } from '../../ui/PageHeader'

export function AdminOverview() {
  const { data, isLoading, isError } = useAdminOverview()

  return (
    <section>
      <PageHeader
        title="Overview"
        eyebrow="Operate"
        lede="This deployment's orgs, running sandboxes, node inventory, and signup mode."
      />
      {isLoading ? (
        <Skeleton rows={3} />
      ) : isError || !data ? (
        <p className="t-dim">Failed to load the operator overview. Please refresh.</p>
      ) : (
        <>
          <div className="cockpit-grid">
            <StatTile label="Organizations" value={String(data.orgs)} hint="every org on this deployment" />
            <StatTile
              label="Running sandboxes"
              value={String(data.running_sandboxes)}
              hint={data.running_sandboxes_orgs < data.orgs ? `across the first ${data.running_sandboxes_orgs} orgs` : 'across every org'}
            />
            <StatTile
              label="Nodes ready"
              value={data.nodes_total === null ? 'N/A' : `${data.nodes_ready}/${data.nodes_total}`}
              hint={data.nodes_total === null ? 'not available in this deployment' : 'ready / total'}
            />
            <StatTile
              label="Signup mode"
              value={data.signup_mode === 'open' ? 'Open' : 'Waitlist'}
              hint={data.signup_mode === 'open' ? 'self-serve signup is enabled' : 'signups land on the waitlist'}
            />
          </div>
          {data.running_sandboxes_orgs < data.orgs && (
            <p className="t-dim" style={{ fontSize: 'var(--step--1)', margin: 'var(--space-3) 0 0' }}>
              Showing sandboxes from the first {data.running_sandboxes_orgs} of {data.orgs} organizations (oldest
              first; the rollup is capped on large deployments).
            </p>
          )}
          {!!data.failed_orgs && (
            <p className="t-dim" style={{ fontSize: 'var(--step--1)', margin: 'var(--space-3) 0 0' }}>
              {data.failed_orgs} organization{data.failed_orgs === 1 ? '' : 's'} could not be read and{' '}
              {data.failed_orgs === 1 ? 'is' : 'are'} omitted from these figures.
            </p>
          )}
          <nav aria-label="Operate sections" style={{ marginTop: 'var(--space-7)', display: 'flex', gap: 'var(--space-5)' }}>
            <Link to="/admin/orgs" className="t-dim" style={{ color: 'var(--cyan)', textDecoration: 'none' }}>
              View orgs
            </Link>
            <Link to="/admin/nodes" className="t-dim" style={{ color: 'var(--cyan)', textDecoration: 'none' }}>
              View nodes
            </Link>
            <Link to="/admin/waitlist" className="t-dim" style={{ color: 'var(--cyan)', textDecoration: 'none' }}>
              View waitlist
            </Link>
            <Link to="/admin/audit" className="t-dim" style={{ color: 'var(--cyan)', textDecoration: 'none' }}>
              View audit
            </Link>
          </nav>
        </>
      )}
    </section>
  )
}
