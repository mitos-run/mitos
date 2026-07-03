// One sandbox, inspected. A tabbed detail view: Overview and Logs are real;
// Fork tree shows the org-wide tree (no per-sandbox BFF endpoint yet; a
// scoped endpoint is tracked as a follow-up); Terminal, Filesystem, Metrics,
// and Spending are honest coming-soon states with a today alternative where
// one exists. Reads the $id route param.
import { useState } from 'react'
import { useParams } from '@tanstack/react-router'
import { useSandbox, useSetSandboxProject } from '../../data/sandboxes'
import { useProjects } from '../../data/org'
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

function ProjectControl({ id, projectId }: { id: string; projectId?: string }) {
  const { data: projects = [] } = useProjects()
  const setProject = useSetSandboxProject()

  function onChange(e: React.ChangeEvent<HTMLSelectElement>) {
    setProject.mutate({ id, projectId: e.target.value })
  }

  return (
    <div style={{ marginBottom: 'var(--space-4)' }}>
      <label htmlFor="sandbox-project-select" style={{ display: 'block', marginBottom: 'var(--space-2)' }} className="t-dim">
        Project
      </label>
      <select
        id="sandbox-project-select"
        value={projectId ?? ''}
        onChange={onChange}
        disabled={setProject.isPending}
      >
        <option value="">Unassigned</option>
        {projects.map((p) => (
          <option key={p.id} value={p.id}>{p.name}</option>
        ))}
      </select>
      <p className="t-dim" style={{ marginTop: 'var(--space-2)', fontSize: 'var(--text-sm)' }}>
        Per-project access enforcement applies to this sandbox when enabled.
      </p>
    </div>
  )
}

export function SandboxDetail() {
  const { id } = useParams({ from: '/sandboxes/$id' })
  const [tab, setTab] = useState('overview')
  const { data: sb, isLoading, isError } = useSandbox(id)
  if (isError) return <EmptyState title="Sandbox unavailable" body="This sandbox does not exist or is not in this organization." />
  if (isLoading || !sb) return <Skeleton rows={6} />
  return (
    <section>
      <h2 className="mono">{sb.id}</h2>
      <ProjectControl id={sb.id} projectId={sb.project_id} />
      <Tabs tabs={TABS} active={tab} onChange={setTab} ariaLabel="Sandbox detail sections" />
      <div role="tabpanel" id={`panel-${tab}`} aria-labelledby={`tab-${tab}`} tabIndex={0} style={{ marginTop: 'var(--space-5)' }}>
        {tab === 'overview' && <OverviewTab sb={sb} />}
        {tab === 'logs' && <LogsTab id={sb.id} />}
        {tab === 'terminal' && <PlaceholderTab title="Terminal" body="An interactive terminal for this sandbox is coming to the console. Until then, run commands from your own terminal with the CLI: mitos sandbox exec." />}
        {tab === 'files' && <PlaceholderTab title="Filesystem" body="A file browser for this sandbox is coming to the console. Until then, read and write files with the SDK or the CLI." />}
        {tab === 'metrics' && <PlaceholderTab title="Metrics" body="Live CPU and memory charts for this sandbox are coming to the console." />}
        {tab === 'spending' && <PlaceholderTab title="Spending" body="A per-sandbox spending breakdown is coming to the console. Org-wide numbers are on the Usage page today." />}
        {tab === 'forks' && (
          <>
            <p className="t-dim" style={{ marginBottom: 'var(--space-4)' }}>Showing every fork in your organization. A view scoped to just this sandbox is coming.</p>
            <ForkTree />
          </>
        )}
      </div>
    </section>
  )
}
