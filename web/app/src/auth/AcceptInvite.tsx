// AcceptInvite.tsx: the /invite/accept?token=... page, serving BOTH the
// pre-auth and post-auth halves of the invite-accept flow from one
// component (the `authenticated` prop selects which; preauthRouter.tsx and
// the post-auth router.tsx each mount it with the correct value):
//
//   - pre-auth (authenticated=false): looks up the PUBLIC invite summary
//     (org, inviter, masked email hint) via GET /console/invites/lookup (no
//     session required) and offers "Sign in" / "Create account" CTAs. The
//     token is forwarded to signup as ?invite_token= so a fresh account
//     auto-joins the org on verification (see Service.autoJoinPendingInvite
//     server-side); it is also carried on the sign-in CTA's ?next= so a
//     returning user lands back here once OIDC redirect-after-login honors
//     it (a known follow-up: the OIDC callback does not thread ?next=
//     through yet).
//   - post-auth (authenticated=true): shows a confirm-join screen and calls
//     POST /console/invites/accept on confirmation. If the invite was
//     already auto-joined at signup (or by a prior visit), the accept call
//     errors; we re-run the lookup and treat an already-accepted invite as
//     success rather than an error, so the user is never told an operation
//     failed when they are, in fact, already a member.
//
// Brand: Fluorescence tokens only; AuthShell for chrome (same centered
// Card-on-field shell as Login/Signup/Verify for visual parity).
// A11y: single aria-live region drives all state transitions; real buttons
//   with visible focus rings; 44px tap targets; keyboard operable.

import { useEffect, useRef, useState } from 'react'
import { Button } from '@mitos/brand'
import { api } from '../api'
import type { InviteLookupView } from '../api'
import { AuthShell } from './authCommon'

const styles = `
.accept-invite-summary {
  background: var(--field-1, var(--field));
  border: 1px solid var(--hairline);
  border-radius: var(--r-md);
  padding: var(--space-4);
  margin: 0 0 var(--space-5);
}
.accept-invite-summary dt {
  font-size: var(--step--1);
  color: var(--ink-3);
  margin: var(--space-2) 0 0;
}
.accept-invite-summary dt:first-child { margin-top: 0; }
.accept-invite-summary dd {
  margin: 0;
  font-size: var(--step-0);
  color: var(--ink);
}
.accept-invite-actions {
  display: flex;
  flex-direction: column;
  gap: var(--space-3);
}
.accept-invite-actions .btn { width: 100%; min-height: 44px; }
.accept-invite-link-btn {
  display: flex;
  align-items: center;
  justify-content: center;
  width: 100%;
  min-height: 44px;
  padding: var(--space-2) var(--space-4);
  border-radius: var(--r-md);
  text-decoration: none;
  font: inherit;
  font-size: var(--step-0);
  border: 1px solid var(--hairline);
  color: var(--ink);
  transition: border-color var(--dur) var(--ease);
}
.accept-invite-link-btn:hover { border-color: var(--magenta); }
.accept-invite-link-btn:focus-visible {
  outline: 2px solid var(--magenta);
  outline-offset: 2px;
  border-color: transparent;
}
@media (prefers-reduced-motion: reduce) {
  .accept-invite-link-btn { transition: none; }
}
.accept-invite-continue {
  display: flex;
  align-items: center;
  justify-content: center;
  width: 100%;
  min-height: 44px;
  padding: var(--space-2) var(--space-4);
  border-radius: var(--r-md);
  text-decoration: none;
  font: inherit;
  font-size: var(--step-0);
  font-weight: 600;
  background: var(--magenta);
  color: var(--field);
  border: none;
}
`

type AcceptState =
  | { kind: 'loading' }
  | { kind: 'no-token' }
  | { kind: 'invalid' }
  | { kind: 'ready'; look: InviteLookupView }
  | { kind: 'accepting'; look: InviteLookupView }
  | { kind: 'accepted'; orgName: string }
  | { kind: 'error'; message: string }

export type AcceptInviteProps = {
  /** Token from the invite link. Falls back to ?token= in the URL at runtime. */
  token?: string
  /**
   * true when rendered inside the authenticated console (a session already
   * exists): shows the confirm-join screen and performs the accept call.
   * false (the default) renders the pre-auth summary with sign-in /
   * create-account CTAs and never calls the session-required accept
   * endpoint.
   */
  authenticated?: boolean
}

export function AcceptInvite({ token: tokenProp, authenticated = false }: AcceptInviteProps) {
  const token =
    tokenProp ??
    (typeof window !== 'undefined'
      ? new URLSearchParams(window.location.search).get('token') ?? ''
      : '')

  const [state, setState] = useState<AcceptState>(token ? { kind: 'loading' } : { kind: 'no-token' })

  const didLookup = useRef(false)
  useEffect(() => {
    if (!token || didLookup.current) return
    didLookup.current = true
    let cancelled = false
    api
      .inviteLookup(token)
      .then((look) => {
        if (cancelled) return
        if (look.state === 'accepted' && authenticated) {
          setState({ kind: 'accepted', orgName: look.org_name })
        } else {
          setState({ kind: 'ready', look })
        }
      })
      .catch(() => {
        if (!cancelled) setState({ kind: 'invalid' })
      })
    return () => {
      cancelled = true
    }
  }, [token, authenticated])

  async function handleJoin(look: InviteLookupView) {
    setState({ kind: 'accepting', look })
    try {
      await api.acceptInvite(token)
      setState({ kind: 'accepted', orgName: look.org_name })
    } catch {
      // The invite may already have been accepted (e.g. auto-joined at
      // signup verification); re-check before reporting a failure so a
      // genuinely-already-member user is never told the join failed.
      try {
        const fresh = await api.inviteLookup(token)
        if (fresh.state === 'accepted') {
          setState({ kind: 'accepted', orgName: fresh.org_name })
          return
        }
        if (fresh.state === 'expired') {
          setState({ kind: 'error', message: 'This invitation has expired.' })
          return
        }
      } catch {
        // fall through to the generic error below
      }
      setState({ kind: 'error', message: 'This invitation could not be accepted.' })
    }
  }

  const title =
    state.kind === 'accepted'
      ? 'You are in'
      : state.kind === 'invalid' || state.kind === 'no-token'
        ? 'Invitation not found'
        : 'Join organization'

  const subtitle =
    state.kind === 'loading'
      ? 'One moment while we look up your invitation.'
      : state.kind === 'accepted'
        ? `You have joined ${state.orgName}.`
        : state.kind === 'invalid' || state.kind === 'no-token'
          ? 'This link is invalid or has expired.'
          : authenticated
            ? 'Confirm to join this organization.'
            : 'Sign in or create an account to accept.'

  function renderBody() {
    switch (state.kind) {
      case 'loading':
        return (
          <p style={{ textAlign: 'center', color: 'var(--ink-3)', fontSize: 'var(--step--1)', margin: 'var(--space-4) 0' }}>
            Looking up your invitation...
          </p>
        )

      case 'no-token':
      case 'invalid':
        return (
          <p style={{ margin: 0, fontSize: 'var(--step--1)', color: 'var(--ink-3)', textAlign: 'center' }}>
            Ask whoever invited you to send a new invitation, or{' '}
            <a href="/login" style={{ color: 'var(--magenta)' }}>
              sign in
            </a>{' '}
            if you already have an account.
          </p>
        )

      case 'ready': {
        const { look } = state
        const signupHref = `/signup?invite_token=${encodeURIComponent(token)}`
        const signinHref = `/login?next=${encodeURIComponent(`/invite/accept?token=${token}`)}`
        return (
          <>
            <dl className="accept-invite-summary">
              <dt>Organization</dt>
              <dd>{look.org_name || 'an organization'}</dd>
              <dt>Invited by</dt>
              <dd>{look.inviter_name || 'a member'}</dd>
              <dt>Invited email</dt>
              <dd>{look.email_hint}</dd>
              <dt>Role</dt>
              <dd style={{ textTransform: 'capitalize' }}>{look.role}</dd>
            </dl>
            {authenticated ? (
              <div className="accept-invite-actions">
                <Button variant="primary" type="button" onClick={() => void handleJoin(look)}>
                  Join {look.org_name || 'organization'}
                </Button>
              </div>
            ) : (
              <div className="accept-invite-actions">
                <a href={signinHref} className="accept-invite-link-btn">
                  Sign in to accept
                </a>
                <a href={signupHref} className="accept-invite-link-btn">
                  Create an account
                </a>
              </div>
            )}
          </>
        )
      }

      case 'accepting':
        return (
          <p style={{ textAlign: 'center', color: 'var(--ink-3)', fontSize: 'var(--step--1)', margin: 'var(--space-4) 0' }}>
            Joining {state.look.org_name || 'organization'}...
          </p>
        )

      case 'accepted':
        return (
          <a href="/" className="accept-invite-continue">
            Continue to console
          </a>
        )

      case 'error':
        return (
          <p style={{ margin: 0, fontSize: 'var(--step--1)', color: 'var(--ink-3)', textAlign: 'center' }}>
            {state.message}
          </p>
        )
    }
  }

  return (
    <AuthShell title={title} subtitle={subtitle}>
      <style>{styles}</style>
      <div aria-live="polite">{renderBody()}</div>
    </AuthShell>
  )
}
