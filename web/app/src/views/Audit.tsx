// Audit view: filterable event table, retention and export panel, sinks panel.
// Columns: Time, Actor, Action, Target, Detail. Actor shows the resolved
// display name with the raw account id as a dim mono subline (falling back to
// just the id when no name was resolved); Action always shows the raw
// category.operation code as a dim mono badge, for machine-grep parity even
// though Actor/Target now show human names; Target shows target_name,
// falling back to the raw target id.
// Filter covers actor_id, actor_name, action, target, target_name, and detail
// (case-insensitive).
import { useState } from 'react'
import { useAudit } from '../data/account'
import { useAccount } from '../data/account-settings'
import { useAuditRetention, useSetRetention, useAuditSinks, useAddSink, useDeleteSink } from '../data/audit'
import { Skeleton } from '../ui/Skeleton'
import { EmptyState } from '../ui/EmptyState'
import { useToast } from '../ui/Toast'
import { api, type SinkType } from '../api'
import { fmtAbsolute } from '../lib/dates'
import { PageHeader } from '../ui/PageHeader'
import { TableToolbar, useTableFilter } from '../ui/TableToolbar'

function RetentionPanel() {
  const { data: retention, isLoading } = useAuditRetention()
  const setRetention = useSetRetention()
  const { notify } = useToast()
  const [days, setDays] = useState<number | ''>(retention?.days ?? '')

  // Sync local state when retention loads
  const currentDays = retention?.days

  const handleSave = async () => {
    const value = days === '' ? 0 : days
    try {
      await setRetention.mutateAsync(value)
      notify('Retention updated')
    } catch {
      notify('Failed to update retention', 'error')
    }
  }

  return (
    <section className="card" style={{ marginBottom: 'var(--space-6)' }}>
      <h2 style={{ marginBottom: 'var(--space-3)' }}>Retention and export</h2>
      {isLoading ? (
        <Skeleton rows={2} />
      ) : (
        <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-4)' }}>
          <div style={{ display: 'flex', alignItems: 'center', gap: 'var(--space-3)' }}>
            <label htmlFor="retention-days">Retention (days)</label>
            <input
              id="retention-days"
              aria-label="Retention days"
              type="number"
              min={1}
              max={3650}
              value={days === '' ? (currentDays ?? '') : days}
              onChange={(e) => setDays(e.target.value === '' ? '' : Number(e.target.value))}
              style={{ width: '100px' }}
            />
            <button
              onClick={handleSave}
              disabled={setRetention.isPending}
              aria-label="Save retention"
            >
              Save
            </button>
          </div>
          <div>
            <a
              href={api.auditExportUrl()}
              download
              role="link"
              aria-label="Export NDJSON"
              className="btn"
              style={{ display: 'inline-block' }}
            >
              Export NDJSON
            </a>
          </div>
        </div>
      )}
    </section>
  )
}

const SINK_TYPES: SinkType[] = ['webhook', 's3', 'splunk', 'datadog']

function SinksPanel() {
  const { data: sinks = [], isLoading } = useAuditSinks()
  const addSink = useAddSink()
  const deleteSink = useDeleteSink()
  const { notify } = useToast()
  const [newType, setNewType] = useState<SinkType>('webhook')
  const [newEndpoint, setNewEndpoint] = useState('')

  const handleAdd = async () => {
    if (!newEndpoint.startsWith('https://')) {
      notify('Endpoint must start with https://', 'error')
      return
    }
    try {
      await addSink.mutateAsync({ type: newType, endpoint: newEndpoint })
      setNewEndpoint('')
      notify('Sink added')
    } catch (err) {
      // Surfaces the server's actionable cause (e.g. a plan_required 402
      // naming the Team plan) instead of a generic failure message.
      notify(err instanceof Error ? err.message : 'Failed to add sink', 'error')
    }
  }

  const handleDelete = async (id: string) => {
    try {
      await deleteSink.mutateAsync(id)
      notify('Sink removed')
    } catch {
      notify('Failed to remove sink', 'error')
    }
  }

  return (
    <section className="card" style={{ marginBottom: 'var(--space-6)' }}>
      <h2 style={{ marginBottom: 'var(--space-3)' }}>Sinks</h2>
      <p className="t-dim" style={{ fontSize: 'var(--step--1)', marginBottom: 'var(--space-4)' }}>
        Forward audit events to external destinations. Endpoint must be HTTPS.
      </p>

      {isLoading ? (
        <Skeleton rows={3} />
      ) : sinks.length === 0 ? (
        <EmptyState
          title="No sinks configured"
          body="Add a sink to forward audit events to an external destination."
        />
      ) : (
        <div style={{ overflowX: 'auto', marginBottom: 'var(--space-4)' }}>
          <table className="tbl" aria-label="Audit sinks">
            <thead>
              <tr>
                <th scope="col">Type</th>
                <th scope="col">Endpoint</th>
                <th scope="col">Enabled</th>
                <th scope="col"><span className="sr-only">Actions</span></th>
              </tr>
            </thead>
            <tbody>
              {sinks.map((sink) => (
                <tr key={sink.id}>
                  <td className="mono">{sink.type}</td>
                  <td className="mono">{sink.endpoint}</td>
                  <td>{sink.enabled ? 'Yes' : 'No'}</td>
                  <td>
                    <button
                      onClick={() => handleDelete(sink.id)}
                      disabled={deleteSink.isPending}
                      aria-label={`Delete sink ${sink.endpoint}`}
                    >
                      Delete
                    </button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}

      <div style={{ display: 'flex', gap: 'var(--space-3)', alignItems: 'flex-end', flexWrap: 'wrap' }}>
        <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-1)' }}>
          <label htmlFor="sink-type">Type</label>
          <select
            id="sink-type"
            value={newType}
            onChange={(e) => setNewType(e.target.value as SinkType)}
            aria-label="Sink type"
          >
            {SINK_TYPES.map((t) => (
              <option key={t} value={t}>{t}</option>
            ))}
          </select>
        </div>
        <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-1)', flex: 1, minWidth: '200px' }}>
          <label htmlFor="sink-endpoint">Endpoint</label>
          <input
            id="sink-endpoint"
            type="url"
            placeholder="https://..."
            value={newEndpoint}
            onChange={(e) => setNewEndpoint(e.target.value)}
            aria-label="Sink endpoint"
            aria-describedby="sink-endpoint-hint"
          />
          <span id="sink-endpoint-hint" className="t-dim" style={{ fontSize: 'var(--step--2)' }}>
            HTTPS required
          </span>
        </div>
        <button
          onClick={handleAdd}
          disabled={addSink.isPending || !newEndpoint}
          aria-label="Add sink"
        >
          Add
        </button>
      </div>
    </section>
  )
}

export function Audit() {
  const { data: events = [], isLoading } = useAudit()
  const { data: account } = useAccount()
  const { query, setQuery, filtered } = useTableFilter(
    events,
    (e) => `${e.actor_id} ${e.actor_name ?? ''} ${e.action} ${e.target} ${e.target_name ?? ''} ${e.detail}`,
  )

  return (
    <section>
      <PageHeader title="Audit log" lede="Immutable record of org-scoped actions. Filter by actor, action, target, or detail." />

      <RetentionPanel />
      <SinksPanel />

      {isLoading ? (
        <Skeleton rows={5} />
      ) : events.length === 0 ? (
        <EmptyState
          title="No audit events yet"
          body="Actions taken in this org will appear here."
        />
      ) : (
        <div style={{ overflowX: 'auto' }}>
          <TableToolbar query={query} onQueryChange={setQuery} count={filtered.length} noun="events" />
          <table className="tbl" aria-label="Audit log">
            <thead>
              <tr>
                <th scope="col">Time</th>
                <th scope="col">Actor</th>
                <th scope="col">Action</th>
                <th scope="col">Target</th>
                <th scope="col">Detail</th>
              </tr>
            </thead>
            <tbody>
              {filtered.map((e, i) => (
                <tr key={i}>
                  <td className="t-dim">{fmtAbsolute(e.at, account?.locale, account?.timezone)}</td>
                  <td>
                    {e.actor_name ? (
                      <>
                        <div>{e.actor_name}</div>
                        <div className="mono t-dim" style={{ fontSize: 'var(--step--2)' }}>{e.actor_id}</div>
                      </>
                    ) : (
                      <span className="mono">{e.actor_id}</span>
                    )}
                  </td>
                  <td>
                    <span className="mono t-dim" style={{ fontSize: 'var(--step--2)' }}>{e.action}</span>
                  </td>
                  <td>{e.target_name ? e.target_name : <span className="mono">{e.target}</span>}</td>
                  <td>{e.detail}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </section>
  )
}
