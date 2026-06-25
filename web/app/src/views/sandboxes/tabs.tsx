// Sandbox detail tab panels. Overview, Logs, and Fork tree read real BFF data;
// Terminal, Filesystem, Metrics, and Spending render an honest placeholder naming
// the surface they will read (those need new BFF endpoints, a later phase).
import type { SandboxView } from '../../api'
import { fmtBytes } from '../../api'
import { useSandboxLogs } from '../../data/sandboxes'
import { EmptyState } from '../../ui/EmptyState'
import { Skeleton } from '../../ui/Skeleton'

export function OverviewTab({ sb }: { sb: SandboxView }) {
  const rows: [string, string][] = [
    ['Template', sb.template], ['Node', sb.node], ['Phase', sb.phase],
    ['vCPUs', String(sb.vcpus)], ['Memory', fmtBytes(sb.mem_bytes)], ['Created', sb.created_at || '-'],
  ]
  return (
    <dl className="kv">
      {rows.map(([k, v]) => (<div key={k} className="kv-row"><dt className="t-dim">{k}</dt><dd className="mono">{v}</dd></div>))}
    </dl>
  )
}

export function LogsTab({ id }: { id: string }) {
  const { data, isLoading, isError } = useSandboxLogs(id)
  if (isError) return <EmptyState title="Logs unavailable" body="The log stream could not be read for this sandbox." />
  if (isLoading) return <Skeleton rows={5} />
  if (!data) return <EmptyState title="No logs yet" body="This sandbox has not emitted any log lines." />
  return <pre className="logs mono">{data}</pre>
}

export function PlaceholderTab({ title, surface }: { title: string; surface: string }) {
  return <EmptyState title={`${title} ships in a later phase`} body={`It will stream over ${surface}. The transport exists; the console tab is a follow-up.`} />
}
