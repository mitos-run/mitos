// Org secrets (#275). Write-only: the value is sent on create and never shown
// again -- the list renders metadata plus fingerprint only. Values are
// encrypted server-side and injected into sandboxes at fork time.
import { useState } from 'react'
import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import { api, type SecretView } from '../api'
import { Skeleton } from '../ui/Skeleton'
import { EmptyState } from '../ui/EmptyState'
import { useToast } from '../ui/Toast'

function useSecrets() {
  return useQuery<SecretView[]>({ queryKey: ['secrets'], queryFn: () => api.secrets() })
}

function useCreateSecret() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (v: { name: string; value: string }) => api.createSecret(v.name, v.value),
    onSuccess: () => void qc.invalidateQueries({ queryKey: ['secrets'] }),
  })
}

function useDeleteSecret() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (name: string) => api.deleteSecret(name),
    onSuccess: () => void qc.invalidateQueries({ queryKey: ['secrets'] }),
  })
}

export function Secrets() {
  const { data: secrets = [], isLoading } = useSecrets()
  const createSecret = useCreateSecret()
  const deleteSecret = useDeleteSecret()
  const { notify } = useToast()

  const [name, setName] = useState('')
  const [value, setValue] = useState('')

  function onSubmit(e: React.FormEvent) {
    e.preventDefault()
    createSecret.mutate(
      { name, value },
      {
        onSuccess: () => {
          setName('')
          setValue('')
          notify('Secret stored', 'ok')
        },
        onError: () => notify('Failed to create secret', 'error'),
      },
    )
  }

  function onDelete(secretName: string) {
    deleteSecret.mutate(secretName, {
      onSuccess: () => notify('Secret deleted', 'ok'),
      onError: () => notify('Failed to delete secret', 'error'),
    })
  }

  return (
    <section>
      <h2>Secrets</h2>
      <p className="t-dim" style={{ fontSize: 'var(--step--1)', marginBottom: 'var(--space-5)' }}>
        Write-only. Values are encrypted server-side and injected into sandboxes; they are never shown again. Rotate, do not read.
      </p>

      <form onSubmit={onSubmit} style={{ marginBottom: 'var(--space-6)' }}>
        <div style={{ display: 'flex', gap: 'var(--space-3)', flexWrap: 'wrap', alignItems: 'flex-end' }}>
          <div>
            <label htmlFor="secret-name" style={{ display: 'block', marginBottom: 'var(--space-1)', fontSize: 'var(--step--1)' }}>
              Name
            </label>
            <input
              id="secret-name"
              className="mono"
              placeholder="e.g. OPENAI_API_KEY"
              value={name}
              onChange={(e) => setName(e.target.value)}
              required
            />
          </div>

          <div>
            <label htmlFor="secret-value" style={{ display: 'block', marginBottom: 'var(--space-1)', fontSize: 'var(--step--1)' }}>
              Value
            </label>
            <input
              id="secret-value"
              type="password"
              className="mono"
              placeholder="secret value"
              value={value}
              onChange={(e) => setValue(e.target.value)}
              required
            />
          </div>

          <button
            type="submit"
            className="btn"
            disabled={!name || !value || createSecret.isPending}
          >
            Create
          </button>
        </div>
      </form>

      {isLoading ? (
        <Skeleton rows={3} />
      ) : secrets.length === 0 ? (
        <EmptyState
          title="No secrets yet"
          body="Store your first secret to inject credentials into sandboxes at fork time."
        />
      ) : (
        <div style={{ overflowX: 'auto' }}>
          <table className="tbl" aria-label="Secrets">
            <thead>
              <tr>
                <th scope="col">Name</th>
                <th scope="col">Provider</th>
                <th scope="col">Mode</th>
                <th scope="col">Version</th>
                <th scope="col">Fingerprint</th>
                <th scope="col"><span className="sr-only">Actions</span></th>
              </tr>
            </thead>
            <tbody>
              {secrets.map((s) => (
                <tr key={s.name}>
                  <td className="mono">{s.name}</td>
                  <td>{s.provider}</td>
                  <td>{s.mode}</td>
                  <td>{s.version}</td>
                  <td className="mono t-dim">{s.fingerprint}</td>
                  <td>
                    <button
                      className="btn btn-ghost"
                      onClick={() => onDelete(s.name)}
                      aria-label={`Delete ${s.name}`}
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
    </section>
  )
}
