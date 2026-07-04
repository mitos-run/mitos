// Sandbox detail tab panels. Overview, Logs, Fork tree, and RunCommand read
// real BFF data; Filesystem, Metrics, and Spending render an honest
// coming-soon state. That copy is user-facing: plain language, what to use
// today, and no internal endpoint or roadmap-phase references.
import { useState } from 'react'
import { Button } from '@mitos/brand'
import type { SandboxView } from '../../api'
import { fmtBytes } from '../../api'
import { useSandboxLogs, useExecSandbox } from '../../data/sandboxes'
import { useLogStream } from '../../data/useLogStream'
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

// LogsTab shows the one-shot log snapshot by default (GET .../logs, unchanged
// behavior); the Live toggle switches to useLogStream's EventSource tail
// (GET .../logs/stream, SSE) so the pane keeps appending new lines while open.
// Turning Live off (or leaving the tab) closes the stream via useLogStream's
// own cleanup.
export function LogsTab({ id }: { id: string }) {
  const [live, setLive] = useState(false)
  const snapshot = useSandboxLogs(id)
  const stream = useLogStream(id, live)

  return (
    <div>
      <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: 'var(--space-3)' }}>
        <span className="t-dim" style={{ fontSize: 'var(--step--1)' }}>
          {live ? (stream.connected ? 'Live: connected' : 'Live: reconnecting...') : 'Snapshot'}
        </span>
        <label style={{ display: 'flex', alignItems: 'center', gap: 'var(--space-2)', fontSize: 'var(--step--1)' }}>
          <input
            type="checkbox"
            checked={live}
            onChange={(e) => setLive(e.target.checked)}
            aria-label="Live logs"
          />
          Live
        </label>
      </div>
      {live ? (
        stream.lines.length === 0 ? (
          <EmptyState
            title={stream.connected ? 'No log lines yet' : 'Connecting...'}
            body={stream.connected ? 'Waiting for this sandbox to emit a log line.' : 'Opening the live log stream.'}
          />
        ) : (
          <pre className="logs mono">{stream.lines.join('\n')}</pre>
        )
      ) : snapshot.isError ? (
        <EmptyState title="Logs unavailable" body="The log stream could not be read for this sandbox." />
      ) : snapshot.isLoading ? (
        <Skeleton rows={5} />
      ) : !snapshot.data ? (
        <EmptyState title="No logs yet" body="This sandbox has not emitted any log lines." />
      ) : (
        <pre className="logs mono">{snapshot.data}</pre>
      )}
    </div>
  )
}

// RunCommandTab: the "Terminal" tab today. Runs ONE command via the exec
// endpoint and shows its stdout/stderr/exit code; it is honest about NOT
// being an interactive terminal (no PTY, no shell state between runs) so it
// never overclaims what it can do.
export function RunCommandTab({ id }: { id: string }) {
  const [cmd, setCmd] = useState('')
  const exec = useExecSandbox()

  async function handleSubmit(e: React.FormEvent) {
    e.preventDefault()
    if (!cmd.trim() || exec.isPending) return
    exec.mutate({ id, cmd })
  }

  return (
    <div>
      <p className="t-dim" style={{ marginBottom: 'var(--space-4)' }}>
        Full PTY is coming; this runs one command via exec and shows its output.
      </p>
      <form onSubmit={handleSubmit} style={{ display: 'flex', gap: 'var(--space-3)', marginBottom: 'var(--space-4)' }}>
        <label htmlFor="run-command-input" className="sr-only">Command</label>
        <input
          id="run-command-input"
          className="mono"
          style={{ flex: 1 }}
          placeholder="echo hello"
          value={cmd}
          onChange={(e) => setCmd(e.target.value)}
          disabled={exec.isPending}
        />
        <Button type="submit" variant="primary" disabled={!cmd.trim() || exec.isPending}>
          {exec.isPending ? 'Running...' : 'Run'}
        </Button>
      </form>
      {exec.isError && (
        <p role="alert" style={{ color: 'var(--red, var(--magenta))', fontSize: 'var(--step--1)', marginBottom: 'var(--space-4)' }}>
          {exec.error instanceof Error ? exec.error.message : 'The command could not be run.'}
        </p>
      )}
      {exec.data && (
        <div>
          <div className="t-dim" style={{ fontSize: 'var(--step--1)', marginBottom: 'var(--space-2)' }}>
            Exit code {exec.data.exit_code}
          </div>
          {exec.data.stdout && <pre className="logs mono">{exec.data.stdout}</pre>}
          {exec.data.stderr && (
            <pre className="logs mono" style={{ color: 'var(--red, var(--magenta))' }}>{exec.data.stderr}</pre>
          )}
          {!exec.data.stdout && !exec.data.stderr && <p className="t-dim">No output.</p>}
        </div>
      )}
    </div>
  )
}

export function PlaceholderTab({ title, body }: { title: string; body: string }) {
  return <EmptyState title={title} body={body} />
}
