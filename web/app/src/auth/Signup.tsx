// Signup page: sibling of Login. Offers GitHub/Google (social handles new users
// too) and an email form that POSTs to /onboarding/signup. On 202 it switches to
// a "check your email" confirmation state; on any error it shows a non-leaky
// message so the endpoint remains non-enumerating.
//
// Brand: Fluorescence tokens only; no hardcoded hex. Mark (glow) + wordmark.
// Layout: same centered Card-on-field shell as Login for visual sibling parity.
// A11y: real labels, magenta focus ring, aria-live on the status region,
//   44px tap targets, keyboard operable, prefers-reduced-motion respected.

import { useState } from 'react'
import type { FormEvent } from 'react'
import { Button } from '@mitos/brand'
import { post } from '../api'
import { AuthShell, ProviderButtons, resolveNext, useAuthConnectors } from './authCommon'

// ---- Page-specific styles --------------------------------------------------

const styles = `
.signup-email-form .btn {
  width: 100%;
  min-height: 44px;
  margin-top: var(--space-2);
}
.signup-nav-link {
  color: var(--magenta);
  text-decoration: none;
}
.signup-nav-link:hover {
  text-decoration: underline;
}
.signup-nav-link:focus-visible {
  outline: 2px solid var(--magenta);
  outline-offset: 2px;
  border-radius: 2px;
}
.signup-reset-btn {
  background: none;
  border: none;
  padding: 0;
  color: var(--magenta);
  font: inherit;
  font-size: var(--step--1);
  cursor: pointer;
  text-decoration: underline;
  min-height: 44px;
}
.signup-reset-btn:focus-visible {
  outline: 2px solid var(--magenta);
  outline-offset: 2px;
  border-radius: 2px;
}
`

// ---- Signup component -------------------------------------------------------

type SignupState = 'idle' | 'loading' | 'success' | 'error'

export type SignupProps = {
  /** Post-auth redirect destination; falls back to ?next= in the URL. */
  next?: string
  /** Initial email value; falls back to ?email= in the URL (Login handoff). */
  initialEmail?: string
}

export function Signup({ next, initialEmail }: SignupProps) {
  const resolvedNext = resolveNext(next)
  const connectors = useAuthConnectors()

  // Resolve ?email= query param for the Login-to-Signup email handoff.
  const emailDefault =
    initialEmail ??
    (typeof window !== 'undefined'
      ? new URLSearchParams(window.location.search).get('email') ?? ''
      : '')

  const [email, setEmail] = useState(emailDefault)
  const [signupState, setSignupState] = useState<SignupState>('idle')
  const [submittedEmail, setSubmittedEmail] = useState('')

  async function handleSubmit(e: FormEvent<HTMLFormElement>) {
    e.preventDefault()
    const trimmed = email.trim()
    if (!trimmed) return
    setSignupState('loading')
    try {
      await post<null>('/onboarding/signup', { email: trimmed })
      setSubmittedEmail(trimmed)
      setSignupState('success')
    } catch {
      setSignupState('error')
    }
  }

  function handleReset() {
    setSignupState('idle')
    setEmail('')
    setSubmittedEmail('')
  }

  return (
    <AuthShell title="Create your account" subtitle="No card needed. You start with free credit.">
      <style>{styles}</style>

      {/* Status region: always in the DOM; children render conditionally.
          Screen readers observe content changes, not a region appearing. */}
      <div aria-live="polite" aria-atomic="true">
        {signupState === 'success' && (
          <div
            style={{
              textAlign: 'center',
              padding: 'var(--space-2) 0 var(--space-4)',
            }}
          >
            <p
              style={{
                margin: '0 0 var(--space-3)',
                fontSize: 'var(--step-0)',
                color: 'var(--ink)',
                lineHeight: 'var(--lh-base)',
              }}
            >
              Check your email. We sent a link to{' '}
              <strong>{submittedEmail}</strong> to finish signing up.
            </p>
            <button
              type="button"
              className="signup-reset-btn"
              onClick={handleReset}
            >
              Use a different email
            </button>
          </div>
        )}

        {signupState === 'error' && (
          <p
            style={{
              margin: '0 0 var(--space-4)',
              fontSize: 'var(--step--1)',
              color: 'var(--ink-3)',
              textAlign: 'center',
            }}
          >
            Something went wrong. Try again.
          </p>
        )}
      </div>

      {/* Provider links: only renders buttons for configured providers. */}
      {signupState !== 'success' && (
        <>
          <ProviderButtons next={resolvedNext} connectors={connectors} />

          {/* Email form */}
          <form
            className="signup-email-form"
            onSubmit={handleSubmit}
            aria-label="Sign up with email"
          >
            <div className="form-row">
              <label htmlFor="signup-email">Email</label>
              <input
                id="signup-email"
                type="email"
                name="email"
                autoComplete="email"
                autoFocus
                required
                placeholder="you@example.com"
                value={email}
                onChange={(e) => setEmail(e.target.value)}
              />
            </div>
            <Button
              variant="primary"
              type="submit"
              disabled={signupState === 'loading'}
            >
              {signupState === 'loading' ? 'Sending...' : 'Send me a sign-in link'}
            </Button>
          </form>
        </>
      )}

      {/* Footer: sign-in link */}
      <p
        style={{
          margin: 'var(--space-5) 0 0',
          textAlign: 'center',
          fontSize: 'var(--step--1)',
          color: 'var(--ink-3)',
        }}
      >
        Already have an account?{' '}
        <a href="/login" className="signup-nav-link">
          Sign in
        </a>
      </p>
    </AuthShell>
  )
}
