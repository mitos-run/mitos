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
import { Mark, Button, Card } from '@mitos/brand'
import { post } from '../api'

// ---- Provider glyphs -------------------------------------------------------
// Identical to Login.tsx: inline SVGs, aria-hidden, focusable=false.

function GitHubGlyph() {
  return (
    <svg
      width="16"
      height="16"
      viewBox="0 0 16 16"
      fill="currentColor"
      aria-hidden
      focusable="false"
      style={{ flexShrink: 0 }}
    >
      <path d="M8 0C3.58 0 0 3.58 0 8c0 3.54 2.29 6.53 5.47 7.59.4.07.55-.17.55-.38 0-.19-.01-.82-.01-1.49-2.01.37-2.53-.49-2.69-.94-.09-.23-.48-.94-.82-1.13-.28-.15-.68-.52-.01-.53.63-.01 1.08.58 1.23.82.72 1.21 1.87.87 2.33.66.07-.52.28-.87.51-1.07-1.78-.2-3.64-.89-3.64-3.95 0-.87.31-1.59.82-2.15-.08-.2-.36-1.02.08-2.12 0 0 .67-.21 2.2.82.64-.18 1.32-.27 2-.27.68 0 1.36.09 2 .27 1.53-1.04 2.2-.82 2.2-.82.44 1.1.16 1.92.08 2.12.51.56.82 1.27.82 2.15 0 3.07-1.87 3.75-3.65 3.95.29.25.54.73.54 1.48 0 1.07-.01 1.93-.01 2.2 0 .21.15.46.55.38A8.013 8.013 0 0016 8c0-4.42-3.58-8-8-8z" />
    </svg>
  )
}

function GoogleGlyph() {
  // Standard Google G: four-color per Google brand guidelines; provider glyph
  // only, not a Mitos brand element, so canonical colors apply.
  return (
    <svg
      width="16"
      height="16"
      viewBox="0 0 18 18"
      aria-hidden
      focusable="false"
      style={{ flexShrink: 0 }}
    >
      <path
        d="M17.64 9.2c0-.637-.057-1.251-.164-1.84H9v3.481h4.844a4.14 4.14 0 01-1.796 2.716v2.259h2.908C16.658 14.129 17.64 11.82 17.64 9.2z"
        fill="#4285F4"
      />
      <path
        d="M9 18c2.43 0 4.467-.806 5.956-2.184l-2.908-2.259c-.806.54-1.837.86-3.048.86-2.344 0-4.328-1.584-5.036-3.711H.957v2.332A8.997 8.997 0 009 18z"
        fill="#34A853"
      />
      <path
        d="M3.964 10.706A5.41 5.41 0 013.682 9c0-.593.102-1.17.282-1.706V4.962H.957A8.996 8.996 0 000 9c0 1.452.348 2.827.957 4.038l3.007-2.332z"
        fill="#FBBC05"
      />
      <path
        d="M9 3.58c1.321 0 2.508.454 3.44 1.345l2.582-2.58C13.463.891 11.426 0 9 0A8.997 8.997 0 00.957 4.962L3.964 7.294C4.672 5.163 6.656 3.58 9 3.58z"
        fill="#EA4335"
      />
    </svg>
  )
}

// ---- Scoped styles ---------------------------------------------------------

const styles = `
.signup-link-btn {
  display: flex;
  align-items: center;
  justify-content: center;
  gap: var(--space-2);
  width: 100%;
  min-height: 44px;
  padding: var(--space-2) var(--space-4);
  border-radius: var(--r-md);
  font: inherit;
  font-size: var(--step-0);
  text-decoration: none;
  cursor: pointer;
  transition: border-color var(--dur) var(--ease), box-shadow var(--dur) var(--ease);
  background: transparent;
  color: var(--ink);
  border: 1px solid var(--hairline);
}
.signup-link-btn:hover {
  border-color: var(--magenta);
}
.signup-link-btn:focus-visible {
  outline: 2px solid var(--magenta);
  outline-offset: 2px;
  border-color: transparent;
}
@media (prefers-reduced-motion: reduce) {
  .signup-link-btn {
    transition: none;
  }
}
.signup-divider {
  display: flex;
  align-items: center;
  gap: var(--space-3);
  margin: var(--space-5) 0;
}
.signup-divider-line {
  flex: 1;
  height: 1px;
  background: var(--hairline);
  border: none;
  margin: 0;
}
.signup-divider-label {
  color: var(--ink-3);
  font-size: var(--step--1);
  text-transform: uppercase;
  letter-spacing: 0.06em;
}
.signup-email-form .btn {
  width: 100%;
  min-height: 44px;
  margin-top: var(--space-2);
}
.signup-email-form .btn:focus-visible {
  outline: 2px solid var(--magenta);
  outline-offset: 2px;
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
  // Resolve ?next= query param: prop takes precedence (testable), then live URL.
  const resolvedNext =
    next ??
    (typeof window !== 'undefined'
      ? new URLSearchParams(window.location.search).get('next') ?? ''
      : '')

  // Resolve ?email= query param for the Login-to-Signup email handoff.
  const emailDefault =
    initialEmail ??
    (typeof window !== 'undefined'
      ? new URLSearchParams(window.location.search).get('email') ?? ''
      : '')

  const [email, setEmail] = useState(emailDefault)
  const [signupState, setSignupState] = useState<SignupState>('idle')
  const [submittedEmail, setSubmittedEmail] = useState('')

  function connectorHref(connector: string): string {
    const params = new URLSearchParams({ connector })
    if (resolvedNext) params.set('next', resolvedNext)
    return `/auth/login?${params.toString()}`
  }

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
    <>
      <style>{styles}</style>
      <div
        style={{
          width: '100%',
          maxWidth: '400px',
        }}
      >
        <Card>
          {/* ---- Brand mark + wordmark ---- */}
          <div
            style={{
              display: 'flex',
              flexDirection: 'column',
              alignItems: 'center',
              marginBottom: 'var(--space-6)',
            }}
          >
            <div
              style={{
                display: 'flex',
                alignItems: 'center',
                gap: 'var(--space-2)',
                marginBottom: 'var(--space-5)',
              }}
            >
              <Mark size={28} glow />
              <span
                style={{
                  fontWeight: 600,
                  fontSize: 'var(--step-1)',
                  letterSpacing: 'var(--track-display)',
                  color: 'var(--ink)',
                }}
              >
                mitos
              </span>
            </div>

            <h1
              style={{
                fontSize: 'var(--step-2)',
                fontWeight: 400,
                letterSpacing: 'var(--track-display)',
                lineHeight: 'var(--lh-tight)',
                margin: '0 0 var(--space-2)',
                textAlign: 'center',
              }}
            >
              Create your account
            </h1>
            <p
              style={{
                margin: 0,
                color: 'var(--ink-3)',
                fontSize: 'var(--step--1)',
                textAlign: 'center',
              }}
            >
              No card needed. You start with free credit.
            </p>
          </div>

          {/* ---- Status region: confirmation or error (aria-live for screen readers) ---- */}
          <div
            aria-live="polite"
            aria-atomic="true"
            style={{ display: signupState === 'success' || signupState === 'error' ? undefined : 'none' }}
          >
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

          {/* ---- Provider links (always visible; social signs up new users too) ---- */}
          {signupState !== 'success' && (
            <>
              <div
                style={{
                  display: 'flex',
                  flexDirection: 'column',
                  gap: 'var(--space-3)',
                }}
              >
                <a
                  href={connectorHref('github')}
                  className="signup-link-btn"
                >
                  <GitHubGlyph />
                  Continue with GitHub
                </a>

                <a
                  href={connectorHref('google')}
                  className="signup-link-btn"
                >
                  <GoogleGlyph />
                  Continue with Google
                </a>
              </div>

              {/* ---- Hairline divider ---- */}
              <div className="signup-divider" aria-hidden>
                <hr className="signup-divider-line" />
                <span className="signup-divider-label">or</span>
                <hr className="signup-divider-line" />
              </div>

              {/* ---- Email form ---- */}
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

          {/* ---- Footer: sign-in link ---- */}
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
        </Card>
      </div>
    </>
  )
}
