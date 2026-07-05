// InviteNudge: a one-time "Bring your team" card shown on the Overview once
// the org has real first activity (never during FirstRun itself) and still
// has exactly one member. Dismissing it persists to localStorage so it never
// reappears, mirroring the try/catch guard in appearance.ts so a restricted
// storage context degrades to "shows every time" rather than throwing.
//
// Gating: caps.teams (members/roles is on in both editions, but still gated
// through the server-advertised capability, never hardcoded) and
// members.length === 1. No em or en dashes.
import { useState } from 'react'
import { Button, Card } from '@mitos/brand'
import { useCapabilities } from '../../data/query'
import { useMembers } from '../../data/org'
import { InviteModal } from '../members/InviteModal'

const DISMISS_KEY = 'mitos-invite-nudge-dismissed'

function isDismissed(): boolean {
  try {
    return localStorage.getItem(DISMISS_KEY) === '1'
  } catch {
    return false
  }
}

function persistDismissed(): void {
  try {
    localStorage.setItem(DISMISS_KEY, '1')
  } catch {
    // Storage unavailable: the nudge just reappears next session, not fatal.
  }
}

export function InviteNudge() {
  const { data: caps } = useCapabilities()
  const { data: members } = useMembers()
  const [dismissed, setDismissed] = useState(isDismissed)
  const [inviteOpen, setInviteOpen] = useState(false)

  if (dismissed) return null
  if (!caps?.teams) return null
  if (!members || members.length !== 1) return null

  return (
    <>
      <Card style={{ marginTop: 'var(--space-6)' }}>
        <h2 style={{ marginTop: 0, marginBottom: 'var(--space-2)' }}>Bring your team</h2>
        <p className="t-dim" style={{ marginTop: 0, marginBottom: 'var(--space-4)' }}>
          You are the only member of this organization. Invite teammates so they
          can share sandboxes, keys, and spend visibility.
        </p>
        <div style={{ display: 'flex', gap: 'var(--space-3)' }}>
          <Button variant="primary" onClick={() => setInviteOpen(true)}>
            Invite people
          </Button>
          <button
            type="button"
            className="btn btn-ghost"
            onClick={() => {
              persistDismissed()
              setDismissed(true)
            }}
          >
            Dismiss
          </button>
        </div>
      </Card>

      {inviteOpen && <InviteModal onClose={() => setInviteOpen(false)} />}
    </>
  )
}
