// Live sandboxes list: polls every 10 s via useSandboxes, renders one row per
// sandbox with an optimistic terminate action (the row disappears instantly and
// rolls back if the server rejects). Each sandbox id links to its detail view.
import { Link } from '@tanstack/react-router'
import { StatusDot, Entity } from '@mitos/brand'
import { useSandboxes, useTerminateSandbox } from '../../data/sandboxes'
import { fmtBytes } from '../../api'
import { Skeleton } from '../../ui/Skeleton'
import { EmptyState } from '../../ui/EmptyState'
import { useToast } from '../../ui/Toast'
import { PageHeader } from '../../ui/PageHeader'
import { TableToolbar, useTableFilter } from '../../ui/TableToolbar'

function phaseEntity(phase: string): Entity {
  if (phase === 'Running') return 'ready'
  if (phase === 'Paused') return 'warn'
  return 'parent'
}

export function SandboxList() {
  const { data: rows = [], isLoading, isError } = useSandboxes()
  const terminate = useTerminateSandbox()
  const { notify } = useToast()
  const { query, setQuery, filtered } = useTableFilter(rows, (s) => `${s.id} ${s.template} ${s.node} ${s.phase}`)

  function onTerminate(id: string) {
    terminate.mutate(id, {
      onSuccess: () => notify(`Terminated ${id}`, 'ok'),
      onError: () => notify(`Failed to terminate ${id}`, 'error'),
    })
  }

  if (isError) {
    return (
      <section>
        <PageHeader title="Sandboxes" lede="Live sandboxes for this org. You see the sandboxes you can access." />
        <EmptyState title="Sandboxes unavailable" body="The sandbox list could not be loaded." />
      </section>
    )
  }

  if (isLoading) {
    return (
      <section>
        <PageHeader title="Sandboxes" lede="Live sandboxes for this org. You see the sandboxes you can access." />
        <Skeleton rows={4} />
      </section>
    )
  }

  if (rows.length === 0) {
    return (
      <section>
        <PageHeader title="Sandboxes" lede="Live sandboxes for this org. You see the sandboxes you can access." />
        <EmptyState title="No live sandboxes" body="Fork your first sandbox to see it here." />
      </section>
    )
  }

  return (
    <section>
      <PageHeader title="Sandboxes" lede="Live sandboxes for this org. You see the sandboxes you can access." />
      <div style={{ overflowX: 'auto' }}>
        <TableToolbar query={query} onQueryChange={setQuery} count={filtered.length} noun="sandboxes" />
        <table className="tbl" aria-label="Live sandboxes">
          <thead>
            <tr>
              <th scope="col">ID</th>
              <th scope="col">Template</th>
              <th scope="col">Node</th>
              <th scope="col">Phase</th>
              <th scope="col">Project</th>
              <th scope="col">vCPUs</th>
              <th scope="col">Memory</th>
              <th scope="col"><span className="sr-only">Actions</span></th>
            </tr>
          </thead>
          <tbody>
            {filtered.map((s) => (
              <tr key={s.id}>
                <td className="mono">
                  <Link to="/sandboxes/$id" params={{ id: s.id }}>{s.id}</Link>
                </td>
                <td>{s.template}</td>
                <td>{s.node}</td>
                <td>
                  <StatusDot entity={phaseEntity(s.phase)} />
                  {' '}{s.phase}
                </td>
                <td>{s.project_id ? s.project_id : <span className="t-dim">Unassigned</span>}</td>
                <td>{s.vcpus}</td>
                <td className="mono">{fmtBytes(s.mem_bytes)}</td>
                <td>
                  <button
                    className="btn btn-ghost"
                    onClick={() => onTerminate(s.id)}
                    aria-label={`Terminate ${s.id}`}
                  >
                    Terminate
                  </button>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </section>
  )
}
