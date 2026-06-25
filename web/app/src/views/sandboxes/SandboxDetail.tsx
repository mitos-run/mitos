// One sandbox, inspected. A tabbed detail view: Overview and Logs are real;
// Fork tree shows the org-wide tree (no per-sandbox BFF endpoint yet; a
// scoped endpoint is tracked as a follow-up); Terminal, Filesystem, Metrics,
// and Spending are honest placeholders. Reads the $id route param.
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
  const { id } = useParams({ from: '/sandboxes/$id' })
  const [tab, setTab] = useState('overview')
  const { data: sb, isLoading, isError } = useSandbox(id)
  if (isError) return <EmptyState title="Sandbox unavailable" body="This sandbox does not exist or is not in this organization." />
  if (isLoading || !sb) return <Skeleton rows={6} />
  return (
    <section>
      <h2 className="mono">{sb.id}</h2>
      <Tabs tabs={TABS} active={tab} onChange={setTab} ariaLabel="Sandbox detail sections" />
      <div role="tabpanel" id={`panel-${tab}`} aria-labelledby={`tab-${tab}`} tabIndex={0} style={{ marginTop: 'var(--space-5)' }}>
        {tab === 'overview' && <OverviewTab sb={sb} />}
        {tab === 'logs' && <LogsTab id={sb.id} />}
        {tab === 'terminal' && <PlaceholderTab title="Terminal" surface="the existing PTY transport" />}
        {tab === 'files' && <PlaceholderTab title="Filesystem" surface="the existing files API" />}
        {tab === 'metrics' && <PlaceholderTab title="Metrics" surface="the guest telemetry pipeline" />}
        {tab === 'spending' && <PlaceholderTab title="Spending" surface="the usage and cost pipeline" />}
        {tab === 'forks' && (
          <>
            <p className="t-dim" style={{ marginBottom: 'var(--space-4)' }}>Org-wide fork tree. Per-sandbox scoping requires a new BFF endpoint and ships in a later phase.</p>
            <ForkTree />
          </>
        )}
      </div>
    </section>
  )
}
