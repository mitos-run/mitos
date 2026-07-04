// Members view: table of org members with account, role badge + role select
// (accessible, per-row), joined date, and remove; Invite button + modal; a
// Pending invites section showing state/expiry with resend/revoke. Supports
// loading, empty, and error states; the empty state doubles as the invite CTA.
import { useState } from 'react'
import { Button } from '@mitos/brand'
import { useMembers, useSetRole, useRemoveMember, useInvites, useRevokeInvite, useResendInvite } from '../data/org'
import { useAccount } from '../data/account-settings'
import { Skeleton } from '../ui/Skeleton'
import { EmptyState } from '../ui/EmptyState'
import { useToast } from '../ui/Toast'
import type { Role } from '../api'
import { fmtAbsolute } from '../lib/dates'
import { PageHeader } from '../ui/PageHeader'
import { TableToolbar, useTableFilter } from '../ui/TableToolbar'
import { InviteModal } from './members/InviteModal'

const ROLES: Role[] = ['owner', 'admin', 'billing', 'member', 'viewer']

export function Members() {
  const { data: members = [], isLoading, isError } = useMembers()
  const { data: account } = useAccount()
  const setRole = useSetRole()
  const removeMember = useRemoveMember()
  const { data: invites = [], isLoading: invitesLoading } = useInvites()
  const revokeInvite = useRevokeInvite()
  const resendInvite = useResendInvite()
  const { notify } = useToast()
  const { query, setQuery, filtered } = useTableFilter(
    members,
    (m) => `${m.account_id} ${m.display_name ?? ''} ${m.email ?? ''} ${m.role}`,
  )
  const [inviteOpen, setInviteOpen] = useState(false)
  const [confirmRemove, setConfirmRemove] = useState<{ accountId: string; label: string } | null>(null)

  function onRoleChange(accountId: string, role: Role) {
    setRole.mutate(
      { accountId, role },
      {
        onSuccess: () => notify('Role updated', 'ok'),
        onError: () => notify('Failed to update role', 'error'),
      },
    )
  }

  function onConfirmRemove() {
    if (!confirmRemove) return
    const target = confirmRemove
    setConfirmRemove(null)
    removeMember.mutate(target.accountId, {
      onSuccess: () => notify(`Removed ${target.label}`, 'ok'),
      onError: () => notify(`Failed to remove ${target.label}`, 'error'),
    })
  }

  function onRevokeInvite(id: string, email: string) {
    revokeInvite.mutate(id, {
      onSuccess: () => notify(`Revoked invitation to ${email}`, 'ok'),
      onError: () => notify('Failed to revoke invitation', 'error'),
    })
  }

  function onResendInvite(id: string, email: string) {
    resendInvite.mutate(id, {
      onSuccess: () => notify(`Resent invitation to ${email}`, 'ok'),
      onError: () => notify('Failed to resend invitation', 'error'),
    })
  }

  // Only pending (or lazily-expired) invites belong in the "pending" section;
  // accepted/revoked rows are history, not action items, and are left off
  // this view (the audit log already records them).
  const pendingInvites = invites.filter((i) => i.state === 'pending' || i.state === 'expired')

  return (
    <section>
      <PageHeader
        title="Members"
        lede="Manage org membership and roles. Changes take effect immediately."
        actions={<Button variant="primary" onClick={() => setInviteOpen(true)}>Invite people</Button>}
      />

      {isLoading ? (
        <Skeleton rows={3} />
      ) : isError ? (
        <p className="t-dim">Failed to load members. Please refresh.</p>
      ) : members.length === 0 ? (
        <EmptyState
          title="No members"
          body="Invite your team to start collaborating on this org."
          action={{ label: 'Invite people', onClick: () => setInviteOpen(true) }}
        />
      ) : (
        <div style={{ overflowX: 'auto' }}>
          <TableToolbar query={query} onQueryChange={setQuery} count={filtered.length} noun="members" />
          <table className="tbl" aria-label="Members">
            <thead>
              <tr>
                <th scope="col">Account</th>
                <th scope="col">Role</th>
                <th scope="col">Joined</th>
                <th scope="col"><span className="sr-only">Actions</span></th>
              </tr>
            </thead>
            <tbody>
              {filtered.map((m) => {
                // The server joins each member's account for a name and email;
                // fall back to the bare account id when neither is known (an
                // older server, or a lookup miss).
                const primary = m.display_name || m.email || m.account_id
                const secondary = m.display_name && m.email ? m.email : undefined
                const isSelf = account?.account_id === m.account_id
                return (
                <tr key={m.account_id}>
                  <td>
                    <div>{primary}</div>
                    {secondary && (
                      <div className="t-dim" style={{ fontSize: 'var(--step--2)' }}>{secondary}</div>
                    )}
                  </td>
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
                  <td className="t-dim">{fmtAbsolute(m.created_at, account?.locale, account?.timezone)}</td>
                  <td>
                    <button
                      className="btn btn-ghost"
                      onClick={() => setConfirmRemove({ accountId: m.account_id, label: primary })}
                      aria-label={isSelf ? 'Leave this organization' : `Remove ${primary}`}
                    >
                      {isSelf ? 'Leave' : 'Remove'}
                    </button>
                  </td>
                </tr>
                )
              })}
            </tbody>
          </table>
        </div>
      )}

      <div style={{ marginTop: 'var(--space-8)' }}>
        <h2 style={{ marginBottom: 'var(--space-2)' }}>Pending invitations</h2>
        {invitesLoading ? (
          <Skeleton rows={2} />
        ) : pendingInvites.length === 0 ? (
          <p className="t-dim">No pending invitations.</p>
        ) : (
          <div style={{ overflowX: 'auto' }}>
            <table className="tbl" aria-label="Pending invitations">
              <thead>
                <tr>
                  <th scope="col">Email</th>
                  <th scope="col">Role</th>
                  <th scope="col">Status</th>
                  <th scope="col">Expires</th>
                  <th scope="col"><span className="sr-only">Actions</span></th>
                </tr>
              </thead>
              <tbody>
                {pendingInvites.map((inv) => (
                  <tr key={inv.id}>
                    <td>{inv.email}</td>
                    <td>
                      <span className={`role-badge role-${inv.role}`}>{inv.role}</span>
                    </td>
                    <td className="t-dim">{inv.state === 'expired' ? 'Expired' : 'Pending'}</td>
                    <td className="t-dim">{fmtAbsolute(inv.expires_at, account?.locale, account?.timezone)}</td>
                    <td style={{ display: 'flex', gap: 'var(--space-2)' }}>
                      <button
                        className="btn btn-ghost"
                        onClick={() => onResendInvite(inv.id, inv.email)}
                        aria-label={`Resend invitation to ${inv.email}`}
                      >
                        Resend
                      </button>
                      <button
                        className="btn btn-ghost"
                        onClick={() => onRevokeInvite(inv.id, inv.email)}
                        aria-label={`Revoke invitation to ${inv.email}`}
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
      </div>

      {inviteOpen && <InviteModal onClose={() => setInviteOpen(false)} />}

      {confirmRemove && (
        <div
          role="alertdialog"
          aria-modal="true"
          aria-labelledby="confirm-remove-title"
          className="card"
          style={{
            position: 'fixed',
            inset: 0,
            margin: 'auto',
            width: '100%',
            maxWidth: '420px',
            height: 'fit-content',
            padding: 'var(--space-6)',
            zIndex: 100,
          }}
        >
          <h2 id="confirm-remove-title" style={{ marginTop: 0 }}>
            Remove {confirmRemove.label}?
          </h2>
          <p className="t-dim">
            {confirmRemove.accountId === account?.account_id
              ? 'You will lose access to this organization immediately.'
              : 'They will lose access to this organization immediately.'}
          </p>
          <div style={{ display: 'flex', gap: 'var(--space-3)', justifyContent: 'flex-end', marginTop: 'var(--space-5)' }}>
            <button className="btn btn-ghost" onClick={() => setConfirmRemove(null)}>
              Cancel
            </button>
            <button className="btn" onClick={onConfirmRemove}>
              Remove
            </button>
          </div>
        </div>
      )}
    </section>
  )
}
