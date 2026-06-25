// Per-project detail view: shows the project's members and lets admins
// assign or revoke per-project roles. Reached by clicking a project name in
// the Projects list. The route param is the project id.
import { useState } from 'react'
import { useParams } from '@tanstack/react-router'
import { useProjects } from '../../data/org'
import { useProjectMembers, useAssignProjectMember, useRevokeProjectMember } from '../../data/projectmembers'
import { Skeleton } from '../../ui/Skeleton'
import { useToast } from '../../ui/Toast'
import { PageHeader } from '../../ui/PageHeader'
import type { Role } from '../../api'

const ROLES: Role[] = ['owner', 'admin', 'billing', 'member', 'viewer']

export function ProjectDetail() {
  const { id } = useParams({ from: '/projects/$id' })
  const { data: projects = [] } = useProjects()
  const { data: members = [], isLoading } = useProjectMembers(id)
  const assign = useAssignProjectMember(id)
  const revoke = useRevokeProjectMember(id)
  const { notify } = useToast()

  const [accountId, setAccountId] = useState('')
  const [role, setRole] = useState<Role>('viewer')

  const project = projects.find((p) => p.id === id)
  const title = project ? project.name : `Project ${id}`

  function onAssign(e: React.FormEvent) {
    e.preventDefault()
    assign.mutate(
      { accountId, role },
      {
        onSuccess: () => {
          setAccountId('')
          setRole('viewer')
          notify('Member assigned', 'ok')
        },
        onError: () => notify('Failed to assign member', 'error'),
      },
    )
  }

  function onRevoke(memberId: string) {
    revoke.mutate(memberId, {
      onSuccess: () => notify('Member revoked', 'ok'),
      onError: () => notify('Failed to revoke member', 'error'),
    })
  }

  return (
    <section>
      <PageHeader
        title={title}
        lede="Per-project membership. Assign roles scoped to this project."
      />

      <p className="t-dim" style={{ marginBottom: 'var(--space-5)' }}>
        Per-project roles take effect on resources once resources are assigned to projects.
      </p>

      <h2 style={{ marginBottom: 'var(--space-4)' }}>Members</h2>

      {isLoading ? (
        <Skeleton rows={3} />
      ) : members.length === 0 ? (
        <p className="t-dim" style={{ marginBottom: 'var(--space-5)' }}>No project members yet.</p>
      ) : (
        <div style={{ overflowX: 'auto', marginBottom: 'var(--space-6)' }}>
          <table className="tbl" aria-label="Project members">
            <thead>
              <tr>
                <th scope="col">Account</th>
                <th scope="col">Role</th>
                <th scope="col"><span className="sr-only">Actions</span></th>
              </tr>
            </thead>
            <tbody>
              {members.map((m) => (
                <tr key={m.account_id}>
                  <td>{m.account_id}</td>
                  <td>
                    <span className={`role-badge role-${m.role}`}>{m.role}</span>
                  </td>
                  <td>
                    <button
                      className="btn btn-ghost"
                      onClick={() => onRevoke(m.account_id)}
                      aria-label={`Revoke ${m.account_id}`}
                    >
                      Revoke
                    </button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}

      <h2 style={{ marginBottom: 'var(--space-4)' }}>Assign member</h2>

      <form onSubmit={onAssign} style={{ display: 'flex', gap: 'var(--space-3)', flexWrap: 'wrap', alignItems: 'flex-end' }}>
        <div>
          <label htmlFor="assign-account-id" style={{ display: 'block', marginBottom: 'var(--space-1)', fontSize: 'var(--step--1)' }}>
            Account ID
          </label>
          <input
            id="assign-account-id"
            className="mono"
            placeholder="account@example.com"
            value={accountId}
            onChange={(e) => setAccountId(e.target.value)}
            required
          />
        </div>

        <div>
          <label htmlFor="assign-role" style={{ display: 'block', marginBottom: 'var(--space-1)', fontSize: 'var(--step--1)' }}>
            Role
          </label>
          <select
            id="assign-role"
            value={role}
            onChange={(e) => setRole(e.target.value as Role)}
            aria-label="Role"
          >
            {ROLES.map((r) => (
              <option key={r} value={r}>{r}</option>
            ))}
          </select>
        </div>

        <button
          type="submit"
          className="btn"
          disabled={!accountId || assign.isPending}
        >
          Assign
        </button>
      </form>
    </section>
  )
}
