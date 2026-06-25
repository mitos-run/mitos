// Roles view: permission matrix for builtin and custom roles, plus a form to
// add or update custom roles. Builtin roles are read-only. Only owners and
// admins can modify roles; the BFF enforces this and returns 403 for others.
import { useState } from 'react'
import { useRoles, useUpsertRole, useDeleteRole } from '../data/roles'
import { Skeleton } from '../ui/Skeleton'
import { useToast } from '../ui/Toast'
import { PageHeader } from '../ui/PageHeader'
import type { Permission } from '../api'

// The enforced permission vocabulary. The BFF rejects any other value.
export const PERMISSIONS: { key: Permission; label: string }[] = [
  { key: 'members.manage', label: 'Manage members' },
  { key: 'projects.manage', label: 'Manage projects' },
  { key: 'secrets.manage', label: 'Manage secrets' },
  { key: 'settings.manage', label: 'Manage settings, retention, and audit' },
  { key: 'billing.manage', label: 'Manage billing' },
  { key: 'resources.use', label: 'Use resources (sandboxes, keys)' },
  { key: 'read', label: 'Read access' },
]

function PermCell({ has }: { has: boolean }) {
  if (has) {
    return <span aria-label="granted" style={{ color: 'var(--green)', fontWeight: 600 }}>&#10003;</span>
  }
  return <span aria-label="not granted" style={{ color: 'var(--ink-3)' }}>-</span>
}

export function Roles() {
  const { data, isLoading, isError } = useRoles()
  const upsert = useUpsertRole()
  const deleteRole = useDeleteRole()
  const { notify } = useToast()

  const [newName, setNewName] = useState('')
  const [newPerms, setNewPerms] = useState<Set<Permission>>(new Set())

  function togglePerm(p: Permission) {
    setNewPerms((prev) => {
      const next = new Set(prev)
      if (next.has(p)) {
        next.delete(p)
      } else {
        next.add(p)
      }
      return next
    })
  }

  function handleSave(e: React.FormEvent) {
    e.preventDefault()
    if (!newName.trim()) return
    upsert.mutate(
      { name: newName.trim(), permissions: Array.from(newPerms) },
      {
        onSuccess: () => {
          notify('Role saved', 'ok')
          setNewName('')
          setNewPerms(new Set())
        },
        onError: () => notify('Failed to save role', 'error'),
      },
    )
  }

  function handleDelete(name: string) {
    deleteRole.mutate(name, {
      onSuccess: () => notify(`Role "${name}" deleted`, 'ok'),
      onError: () => notify('Failed to delete role', 'error'),
    })
  }

  if (isLoading) return <Skeleton rows={5} />
  if (isError) return <p className="t-dim">Failed to load roles. Please refresh.</p>

  const allRoles = [...(data?.builtins ?? []), ...(data?.custom ?? [])]

  return (
    <section>
      <PageHeader
        title="Roles"
        lede="The permission matrix below shows what each role can do. Builtin roles are read-only. These permissions are enforced by the API; only owners and admins can change roles."
      />

      <p className="t-dim" style={{ fontSize: 'var(--step--1)', marginBottom: 'var(--space-4)' }}>
        These permissions are enforced by the API. Only owners and admins can create or modify custom roles.
      </p>

      <div style={{ overflowX: 'auto', marginBottom: 'var(--space-7)' }}>
        <table className="tbl" aria-label="Permission matrix">
          <thead>
            <tr>
              <th scope="col">Permission</th>
              {allRoles.map((role) => (
                <th scope="col" key={role.name}>{role.name}</th>
              ))}
            </tr>
          </thead>
          <tbody>
            {PERMISSIONS.map(({ key, label }) => (
              <tr key={key}>
                <td>{label}</td>
                {allRoles.map((role) => (
                  <td key={role.name} style={{ textAlign: 'center' }}>
                    <PermCell has={role.permissions.includes(key)} />
                  </td>
                ))}
              </tr>
            ))}
          </tbody>
        </table>
      </div>

      {(data?.custom ?? []).length > 0 && (
        <section className="card" style={{ marginBottom: 'var(--space-6)' }}>
          <h2 style={{ marginBottom: 'var(--space-4)' }}>Custom roles</h2>
          <table className="tbl" aria-label="Custom roles">
            <thead>
              <tr>
                <th scope="col">Name</th>
                <th scope="col">Permissions</th>
                <th scope="col"></th>
              </tr>
            </thead>
            <tbody>
              {(data?.custom ?? []).map((role) => (
                <tr key={role.name}>
                  <td>{role.name}</td>
                  <td className="t-dim">{role.permissions.join(', ')}</td>
                  <td>
                    <button
                      className="btn btn-ghost"
                      aria-label={`Delete ${role.name}`}
                      onClick={() => handleDelete(role.name)}
                      disabled={deleteRole.isPending}
                    >
                      Delete
                    </button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </section>
      )}

      <section className="card">
        <h2 style={{ marginBottom: 'var(--space-4)' }}>New custom role</h2>
        <p className="t-dim" style={{ fontSize: 'var(--step--1)', marginBottom: 'var(--space-4)' }}>
          Only the 7 permissions listed in the matrix are accepted. The BFF rejects any other value.
          Only owners and admins can save roles.
        </p>
        <form onSubmit={handleSave}>
          <div className="form-row">
            <label htmlFor="role-name">Role name</label>
            <input
              id="role-name"
              type="text"
              value={newName}
              onChange={(e) => setNewName(e.target.value)}
              placeholder="e.g. deployer"
              style={{ maxWidth: 320 }}
            />
          </div>

          <fieldset style={{ border: 'none', padding: 0, marginBottom: 'var(--space-4)' }}>
            <legend style={{ color: 'var(--ink-3)', fontSize: 'var(--step--1)', textTransform: 'uppercase', letterSpacing: '0.04em', marginBottom: 'var(--space-2)' }}>
              Permissions
            </legend>
            <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-2)' }}>
              {PERMISSIONS.map(({ key, label }) => {
                const checkId = `new-role-perm-${key}`
                return (
                  <div key={key} style={{ display: 'flex', alignItems: 'center', gap: 'var(--space-2)' }}>
                    <input
                      id={checkId}
                      type="checkbox"
                      checked={newPerms.has(key)}
                      onChange={() => togglePerm(key)}
                    />
                    <label htmlFor={checkId}>{label}</label>
                  </div>
                )
              })}
            </div>
          </fieldset>

          <button
            type="submit"
            className="btn btn-primary"
            disabled={upsert.isPending || !newName.trim()}
          >
            {upsert.isPending ? 'Saving...' : 'Save'}
          </button>
        </form>
      </section>

    </section>
  )
}
