// Instance-operator node inventory: read-only, over the cluster's own k8s
// Node objects. When the deployment has no configured Kubernetes client
// (dev, or a non-cluster install) the server reports available:false; this
// view renders that as an honest state, never an empty-looking "no nodes"
// table that could be mistaken for a cluster with zero nodes.
import { useAdminNodes } from '../../data/admin'
import { Skeleton } from '../../ui/Skeleton'
import { EmptyState } from '../../ui/EmptyState'
import { PageHeader } from '../../ui/PageHeader'
import { TableToolbar, useTableFilter } from '../../ui/TableToolbar'

export function AdminNodes() {
  const { data, isLoading, isError } = useAdminNodes()
  const nodes = data?.nodes ?? []
  const { query, setQuery, filtered } = useTableFilter(nodes, (n) => n.name)

  return (
    <section>
      <PageHeader
        title="Nodes"
        eyebrow="Operate"
        lede="The cluster's Kubernetes nodes: readiness, KVM/dedicated placement, and allocatable capacity."
      />
      {isLoading ? (
        <Skeleton rows={4} />
      ) : isError ? (
        <p className="t-dim">Failed to load the node inventory. Please refresh.</p>
      ) : !data?.available ? (
        <EmptyState
          title="Not available in this deployment"
          body="This deployment has no Kubernetes client configured, so the node inventory cannot be read. This is expected for a non-cluster install (for example sandbox-server) or a local dev environment."
        />
      ) : nodes.length === 0 ? (
        <EmptyState title="No nodes" body="The cluster reports no nodes." />
      ) : (
        <div style={{ overflowX: 'auto' }}>
          <TableToolbar query={query} onQueryChange={setQuery} count={filtered.length} noun="nodes" />
          <table className="tbl" aria-label="Nodes">
            <thead>
              <tr>
                <th scope="col">Node</th>
                <th scope="col">Ready</th>
                <th scope="col">KVM</th>
                <th scope="col">Dedicated</th>
                <th scope="col">Allocatable CPU</th>
                <th scope="col">Allocatable memory</th>
              </tr>
            </thead>
            <tbody>
              {filtered.map((n) => (
                <tr key={n.name}>
                  <td className="mono">{n.name}</td>
                  <td>
                    <span className={`dot ${n.ready ? 'dot-ready' : 'dot-warn'}`} aria-hidden="true" />{' '}
                    {n.ready ? 'Ready' : 'Not ready'}
                  </td>
                  <td>{n.kvm ? 'Yes' : 'No'}</td>
                  <td>{n.dedicated ? 'Yes' : 'No'}</td>
                  <td className="mono">{n.allocatable_cpu}</td>
                  <td className="mono">{n.allocatable_mem}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </section>
  )
}
