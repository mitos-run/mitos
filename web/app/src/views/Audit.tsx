// Audit view: filterable event table, retention and export panel, sinks panel.
// Columns: Time, Actor, Action, Target, Detail.
// Filter covers actor_id, action, target, and detail (case-insensitive).
import { useState } from 'react'
import { useAudit } from '../data/account'
import { useAuditRetention, useSetRetention, useAuditSinks, useAddSink, useDeleteSink } from '../data/audit'
import { Skeleton } from '../ui/Skeleton'
import { EmptyState } from '../ui/EmptyState'
import { useToast } from '../ui/Toast'
import { api, type SinkType } from '../api'
import { PageHeader } from '../ui/PageHeader'

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
    } catch {
      notify('Failed to add sink', 'error')
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
  const [filter, setFilter] = useState('')

  const filtered = filter
    ? events.filter((e) => {
        const q = filter.toLowerCase()
        return (
          e.actor_id.toLowerCase().includes(q) ||
          e.action.toLowerCase().includes(q) ||
          e.target.toLowerCase().includes(q) ||
          e.detail.toLowerCase().includes(q)
        )
      })
    : events

  return (
    <section>
      <PageHeader title="Audit log" lede="Immutable record of org-scoped actions. Filter by actor, action, target, or detail." />

      <RetentionPanel />
      <SinksPanel />

      {isLoading ? (
        <Skeleton rows={5} />
      ) : (
        <>
          <div style={{ marginBottom: 'var(--space-4)' }}>
            <input
              aria-label="Filter audit"
              placeholder="Filter by actor, action, target, or detail"
              value={filter}
              onChange={(e) => setFilter(e.target.value)}
              style={{ width: '100%', maxWidth: '400px' }}
            />
          </div>

          {filtered.length === 0 ? (
            <EmptyState
              title={events.length === 0 ? 'No audit events yet' : 'No matching events'}
              body={events.length === 0 ? 'Actions taken in this org will appear here.' : 'Try a different filter term.'}
            />
          ) : (
            <div style={{ overflowX: 'auto' }}>
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
                      <td className="t-dim">{new Date(e.at).toLocaleString()}</td>
                      <td className="mono">{e.actor_id}</td>
                      <td className="mono">{e.action}</td>
                      <td className="mono">{e.target}</td>
                      <td>{e.detail}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </>
      )}
    </section>
  )
}
