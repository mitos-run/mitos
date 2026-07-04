// API keys view: create form with scopes and TTL, masked table with one-time
// raw key reveal (CopyOnce; never refetched), and optimistic revoke per row.
import { useState } from 'react'
import { useKeys, useCreateKey, useRevokeKey } from '../data/account'
import { useAccount } from '../data/account-settings'
import { CopyOnce } from '../ui/CopyOnce'
import { Skeleton } from '../ui/Skeleton'
import { EmptyState } from '../ui/EmptyState'
import { useToast } from '../ui/Toast'
import type { CreateKeyResult } from '../api'
import { fmtAbsolute } from '../lib/dates'
import { PageHeader } from '../ui/PageHeader'
import { TableToolbar, useTableFilter } from '../ui/TableToolbar'

const TTL_OPTIONS = [
  { label: 'Never expires', value: 0 },
  { label: '30 days', value: 2592000 },
  { label: '90 days', value: 7776000 },
] as const

export function Keys() {
  const { data: keys = [], isLoading } = useKeys()
  const { data: account } = useAccount()
  const createKey = useCreateKey()
  const revokeKey = useRevokeKey()
  const { notify } = useToast()
  const { query, setQuery, filtered } = useTableFilter(keys, (k) => `${k.name} ${k.prefix}`)

  const [name, setName] = useState('')
  const [scopeSandboxes, setScopeSandboxes] = useState(true)
  const [scopeRead, setScopeRead] = useState(false)
  const [ttl, setTtl] = useState(0)
  const [revealed, setRevealed] = useState<CreateKeyResult | null>(null)

  function onSubmit(e: React.FormEvent) {
    e.preventDefault()
    const scopes: string[] = []
    if (scopeSandboxes) scopes.push('sandboxes')
    if (scopeRead) scopes.push('read')
    createKey.mutate(
      { name, scopes, ttlSeconds: ttl },
      {
        onSuccess: (result) => {
          setRevealed(result)
          setName('')
          setScopeSandboxes(true)
          setScopeRead(false)
          setTtl(0)
        },
        onError: () => notify('Failed to create key', 'error'),
      },
    )
  }

  function onRevoke(id: string) {
    revokeKey.mutate(id, {
      onSuccess: () => notify('Key revoked', 'ok'),
      onError: () => notify('Failed to revoke key', 'error'),
    })
  }

  return (
    <section>
      <PageHeader title="API keys" lede="Keys authenticate requests to the Mitos API. The raw key is shown only once on creation; store it immediately." />

      {revealed && (
        <div style={{ marginBottom: 'var(--space-5)' }}>
          <CopyOnce value={revealed.raw_key} label="API key" />
          <div style={{ marginTop: 'var(--space-2)' }}>
            <button
              className="btn btn-ghost"
              onClick={() => setRevealed(null)}
            >
              Dismiss
            </button>
          </div>
        </div>
      )}

      <form onSubmit={onSubmit} style={{ marginBottom: 'var(--space-6)' }}>
        <div style={{ display: 'flex', gap: 'var(--space-3)', flexWrap: 'wrap', alignItems: 'flex-end' }}>
          <div>
            <label htmlFor="key-name" style={{ display: 'block', marginBottom: 'var(--space-1)', fontSize: 'var(--step--1)' }}>
              Name
            </label>
            <input
              id="key-name"
              className="mono"
              placeholder="e.g. ci-bot"
              value={name}
              onChange={(e) => setName(e.target.value)}
              required
            />
          </div>

          <div style={{ display: 'flex', gap: 'var(--space-3)', alignItems: 'center' }}>
            <label style={{ display: 'flex', gap: 'var(--space-1)', alignItems: 'center', cursor: 'pointer' }}>
              <input
                type="checkbox"
                checked={scopeSandboxes}
                onChange={(e) => setScopeSandboxes(e.target.checked)}
              />
              sandboxes
            </label>
            <label style={{ display: 'flex', gap: 'var(--space-1)', alignItems: 'center', cursor: 'pointer' }}>
              <input
                type="checkbox"
                checked={scopeRead}
                onChange={(e) => setScopeRead(e.target.checked)}
              />
              read
            </label>
          </div>

          <div>
            <label htmlFor="key-ttl" style={{ display: 'block', marginBottom: 'var(--space-1)', fontSize: 'var(--step--1)' }}>
              Expiry
            </label>
            <select
              id="key-ttl"
              value={ttl}
              onChange={(e) => setTtl(Number(e.target.value))}
            >
              {TTL_OPTIONS.map((opt) => (
                <option key={opt.value} value={opt.value}>
                  {opt.label}
                </option>
              ))}
            </select>
          </div>

          <button
            type="submit"
            className="btn"
            disabled={!name || createKey.isPending}
          >
            Create key
          </button>
        </div>
      </form>

      {isLoading ? (
        <Skeleton rows={3} />
      ) : keys.length === 0 ? (
        <EmptyState
          title="No API keys"
          body="Create your first key to authenticate API requests."
        />
      ) : (
        <div style={{ overflowX: 'auto' }}>
          <TableToolbar query={query} onQueryChange={setQuery} count={filtered.length} noun="keys" />
          <table className="tbl" aria-label="API keys">
            <thead>
              <tr>
                <th scope="col">Name</th>
                <th scope="col">Prefix</th>
                <th scope="col">Scopes</th>
                <th scope="col">Created</th>
                <th scope="col">Status</th>
                <th scope="col"><span className="sr-only">Actions</span></th>
              </tr>
            </thead>
            <tbody>
              {filtered.map((k) => (
                <tr key={k.id}>
                  <td>{k.name}</td>
                  <td className="mono">{k.prefix}</td>
                  <td>{k.scopes.join(', ')}</td>
                  <td className="t-dim">{fmtAbsolute(k.created_at, account?.locale, account?.timezone)}</td>
                  <td>
                    {k.revoked ? (
                      <span className="t-dim">revoked</span>
                    ) : (
                      <span>active</span>
                    )}
                  </td>
                  <td>
                    {!k.revoked && (
                      <button
                        className="btn btn-ghost"
                        onClick={() => onRevoke(k.id)}
                        aria-label={`Revoke ${k.name}`}
                      >
                        Revoke
                      </button>
                    )}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </section>
  )
}
