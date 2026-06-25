// One sandbox, inspected. A tabbed detail view: Overview, Logs, and a Fork tree
// scoped to this sandbox are real; Terminal, Filesystem, Metrics, Spending are
// honest placeholders. Reads the $id route param.
import { useState } from 'react'
import { useParams } from '@tanstack/react-router'
import { useSandbox } from '../../data/sandboxes'
import { Tabs, type TabDef } from '../../ui/Tabs'
import { Skeleton } from '../../ui/Skeleton'
import { EmptyState } from '../../ui/EmptyState'
import { ForkTree } from '../forktree/ForkTree'
import { OverviewTab, LogsTab, PlaceholderTab } from './tabs'

const TABS: TabDef[] = [
  { key: 'overview', label: 'Overview' }, { key: 'logs', label: 'Logs' }, { key: 'terminal', label: 'Terminal' },
  { key: 'files', label: 'Filesystem' }, { key: 'metrics', label: 'Metrics' }, { key: 'spending', label: 'Spending' },
  { key: 'forks', label: 'Fork tree' },
]

export function SandboxDetail() {
  const { id } = useParams({ strict: false }) as { id: string }
  const [tab, setTab] = useState('overview')
  const { data: sb, isLoading, isError } = useSandbox(id)
  if (isError) return <EmptyState title="Sandbox unavailable" body="This sandbox does not exist or is not in this organization." />
  if (isLoading || !sb) return <Skeleton rows={6} />
  return (
    <section>
      <h2 className="mono">{sb.id}</h2>
      <Tabs tabs={TABS} active={tab} onChange={setTab} />
      <div role="tabpanel" id={`panel-${tab}`} aria-labelledby={`tab-${tab}`} style={{ marginTop: 'var(--space-5)' }}>
        {tab === 'overview' && <OverviewTab sb={sb} />}
        {tab === 'logs' && <LogsTab id={sb.id} />}
        {tab === 'terminal' && <PlaceholderTab title="Terminal" surface="the existing PTY transport" />}
        {tab === 'files' && <PlaceholderTab title="Filesystem" surface="the existing files API" />}
        {tab === 'metrics' && <PlaceholderTab title="Metrics" surface="the guest telemetry pipeline" />}
        {tab === 'spending' && <PlaceholderTab title="Spending" surface="the usage and cost pipeline" />}
        {tab === 'forks' && <ForkTree />}
      </div>
    </section>
  )
}
