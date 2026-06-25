// Members view: table of org members with account, role badge + role select
// (accessible, per-row), and joined date. Supports loading, empty, and error states.
import { useMembers, useSetRole } from '../data/org'
import { Skeleton } from '../ui/Skeleton'
import { EmptyState } from '../ui/EmptyState'
import { useToast } from '../ui/Toast'
import type { Role } from '../api'
import { PageHeader } from '../ui/PageHeader'
import { TableToolbar, useTableFilter } from '../ui/TableToolbar'

const ROLES: Role[] = ['owner', 'admin', 'billing', 'member', 'viewer']

export function Members() {
  const { data: members = [], isLoading, isError } = useMembers()
  const setRole = useSetRole()
  const { notify } = useToast()
  const { query, setQuery, filtered } = useTableFilter(members, (m) => `${m.account_id} ${m.role}`)

  function onRoleChange(accountId: string, role: Role) {
    setRole.mutate(
      { accountId, role },
      {
        onSuccess: () => notify('Role updated', 'ok'),
        onError: () => notify('Failed to update role', 'error'),
      },
    )
  }

  return (
    <section>
      <PageHeader title="Members" lede="Manage org membership and roles. Changes take effect immediately." />

      {isLoading ? (
        <Skeleton rows={3} />
      ) : isError ? (
        <p className="t-dim">Failed to load members. Please refresh.</p>
      ) : members.length === 0 ? (
        <EmptyState title="No members" body="No members found in this org." />
      ) : (
        <div style={{ overflowX: 'auto' }}>
          <TableToolbar query={query} onQueryChange={setQuery} count={filtered.length} noun="members" />
          <table className="tbl" aria-label="Members">
            <thead>
              <tr>
                <th scope="col">Account</th>
                <th scope="col">Role</th>
                <th scope="col">Joined</th>
              </tr>
            </thead>
            <tbody>
              {filtered.map((m) => (
                <tr key={m.account_id}>
                  <td>{m.account_id}</td>
                  <td>
                    <span className={`role-badge role-${m.role}`}>
                      {m.role}
                    </span>
                    <label
                      htmlFor={`role-select-${m.account_id}`}
                      style={{ position: 'absolute', width: 1, height: 1, overflow: 'hidden', clip: 'rect(0,0,0,0)' }}
                    >
                      Role for {m.account_id}
                    </label>
                    <select
                      id={`role-select-${m.account_id}`}
                      value={m.role}
                      onChange={(e) => onRoleChange(m.account_id, e.target.value as Role)}
                      aria-label={`Role for ${m.account_id}`}
                    >
                      {ROLES.map((r) => (
                        <option key={r} value={r}>
                          {r}
                        </option>
                      ))}
                    </select>
                  </td>
                  <td className="t-dim">{new Date(m.created_at).toLocaleDateString()}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </section>
  )
}
