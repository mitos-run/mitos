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

function phaseEntity(phase: string): Entity {
  if (phase === 'Running') return 'ready'
  if (phase === 'Paused') return 'warn'
  return 'parent'
}

export function SandboxList() {
  const { data: rows = [], isLoading, isError } = useSandboxes()
  const terminate = useTerminateSandbox()
  const { notify } = useToast()

  function onTerminate(id: string) {
    terminate.mutate(id, {
      onSuccess: () => notify(`Terminated ${id}`, 'ok'),
      onError: () => notify(`Failed to terminate ${id}`, 'error'),
    })
  }

  if (isError) {
    return (
      <section>
        <h2>Sandboxes</h2>
        <EmptyState title="Sandboxes unavailable" body="The sandbox list could not be loaded." />
      </section>
    )
  }

  if (isLoading) {
    return (
      <section>
        <h2>Sandboxes</h2>
        <Skeleton rows={4} />
      </section>
    )
  }

  if (rows.length === 0) {
    return (
      <section>
        <h2>Sandboxes</h2>
        <EmptyState title="No live sandboxes" body="Fork your first sandbox to see it here." />
      </section>
    )
  }

  return (
    <section>
      <h2>Sandboxes</h2>
      <div style={{ overflowX: 'auto' }}>
        <table className="tbl" aria-label="Live sandboxes">
          <thead>
            <tr>
              <th scope="col">ID</th>
              <th scope="col">Template</th>
              <th scope="col">Node</th>
              <th scope="col">Phase</th>
              <th scope="col">vCPUs</th>
              <th scope="col">Memory</th>
              <th scope="col" />
            </tr>
          </thead>
          <tbody>
            {rows.map((s) => (
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
