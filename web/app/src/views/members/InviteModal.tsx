// InviteModal: send one or more email invitations to join the org at a
// chosen role. Accepts a newline/comma-separated list so inviting a whole
// team is one paste, one submit. Each address is sent as its own
// POST /console/invites call (the server invites one address at a time);
// results are shown per-address so a single bad address (already invited,
// malformed) never silently swallows the rest.
//
// A11y: role="dialog" + aria-modal, labelled by the heading, Escape closes,
// the first field receives focus on open, a visible focus ring on every
// control (inherited from base.css / the shared .btn/.auth-link-btn rules).
import { useEffect, useRef, useState } from 'react'
import { Button } from '@mitos/brand'
import { useCreateInvite } from '../../data/org'
import type { Role } from '../../api'

const ROLES: Role[] = ['admin', 'member', 'billing', 'viewer']

function parseEmails(raw: string): string[] {
  const seen = new Set<string>()
  const out: string[] = []
  for (const part of raw.split(/[\n,]/)) {
    const email = part.trim().toLowerCase()
    if (email && !seen.has(email)) {
      seen.add(email)
      out.push(email)
    }
  }
  return out
}

type SendResult = { email: string; ok: boolean; message?: string }

export type InviteModalProps = {
  onClose: () => void
}

export function InviteModal({ onClose }: InviteModalProps) {
  const createInvite = useCreateInvite()
  const [emailsText, setEmailsText] = useState('')
  const [role, setRole] = useState<Role>('member')
  const [results, setResults] = useState<SendResult[] | null>(null)
  const [sending, setSending] = useState(false)
  const firstFieldRef = useRef<HTMLTextAreaElement>(null)

  useEffect(() => {
    firstFieldRef.current?.focus()
  }, [])

  useEffect(() => {
    function onKeyDown(e: KeyboardEvent) {
      if (e.key === 'Escape') onClose()
    }
    document.addEventListener('keydown', onKeyDown)
    return () => document.removeEventListener('keydown', onKeyDown)
  }, [onClose])

  const emails = parseEmails(emailsText)

  async function handleSubmit(e: React.FormEvent) {
    e.preventDefault()
    if (emails.length === 0 || sending) return
    setSending(true)
    const outcomes: SendResult[] = []
    for (const email of emails) {
      try {
        await createInvite.mutateAsync({ email, role })
        outcomes.push({ email, ok: true })
      } catch (err) {
        outcomes.push({ email, ok: false, message: err instanceof Error ? err.message : 'failed to send' })
      }
    }
    setSending(false)
    setResults(outcomes)
    // Only clear the textarea of addresses that succeeded, so a partial
    // failure leaves the failed ones ready to retry.
    const failed = outcomes.filter((o) => !o.ok).map((o) => o.email)
    setEmailsText(failed.join('\n'))
  }

  const allSucceeded = results !== null && results.every((r) => r.ok)

  return (
    <div
      style={{
        position: 'fixed',
        inset: 0,
        background: 'color-mix(in srgb, black 60%, transparent)',
        display: 'flex',
        alignItems: 'center',
        justifyContent: 'center',
        zIndex: 100,
        padding: 'var(--space-4)',
      }}
      onMouseDown={(e) => {
        if (e.target === e.currentTarget) onClose()
      }}
    >
      <div
        role="dialog"
        aria-modal="true"
        aria-labelledby="invite-modal-title"
        className="card"
        style={{ width: '100%', maxWidth: '480px', padding: 'var(--space-6)' }}
      >
        <h2 id="invite-modal-title" style={{ marginTop: 0, marginBottom: 'var(--space-2)' }}>
          Invite people
        </h2>
        <p className="t-dim" style={{ marginTop: 0, marginBottom: 'var(--space-5)' }}>
          One email per line (or comma-separated). Each invitation expires in 7 days.
        </p>

        <form onSubmit={handleSubmit}>
          <div className="form-row" style={{ marginBottom: 'var(--space-4)' }}>
            <label htmlFor="invite-emails">Email addresses</label>
            <textarea
              id="invite-emails"
              ref={firstFieldRef}
              rows={4}
              placeholder={'ada@example.com\ngrace@example.com'}
              value={emailsText}
              onChange={(e) => {
                setEmailsText(e.target.value)
                setResults(null)
              }}
              style={{ width: '100%', resize: 'vertical', font: 'inherit' }}
            />
          </div>

          <div className="form-row" style={{ marginBottom: 'var(--space-5)' }}>
            <label htmlFor="invite-role">Role</label>
            <select id="invite-role" value={role} onChange={(e) => setRole(e.target.value as Role)}>
              {ROLES.map((r) => (
                <option key={r} value={r}>
                  {r}
                </option>
              ))}
            </select>
          </div>

          {results && (
            <ul aria-live="polite" style={{ listStyle: 'none', padding: 0, margin: '0 0 var(--space-4)', fontSize: 'var(--step--1)' }}>
              {results.map((r) => (
                <li key={r.email} style={{ color: r.ok ? 'var(--ink-2)' : 'var(--red, var(--magenta))' }}>
                  {r.ok ? 'Sent to ' : 'Failed for '}
                  {r.email}
                  {r.message ? `: ${r.message}` : ''}
                </li>
              ))}
            </ul>
          )}

          <div style={{ display: 'flex', gap: 'var(--space-3)', justifyContent: 'flex-end' }}>
            <button type="button" className="btn btn-ghost" onClick={onClose}>
              {allSucceeded ? 'Done' : 'Cancel'}
            </button>
            <Button type="submit" variant="primary" disabled={emails.length === 0 || sending}>
              {sending ? 'Sending...' : `Send invite${emails.length > 1 ? 's' : ''}`}
            </Button>
          </div>
        </form>
      </div>
    </div>
  )
}
