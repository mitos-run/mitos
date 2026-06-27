// Login page: the auth entry point that hands off to GitHub, Google, or the
// email/signup flow. All provider links are plain <a href> full navigations
// to the Go /auth/login endpoint. No client-side router context required.
//
// Brand: Fluorescence tokens only; no hardcoded hex. Mark (glow) + wordmark.
// Layout: centered Card on the --field void, generous --space-* rhythm.
// A11y: real labels, magenta focus ring on interactive controls, keyboard
// operable, sufficient contrast from token palette.

import { useState } from 'react'
import type { FormEvent } from 'react'
import { Mark, Button, Card } from '@mitos/brand'

// ---- Provider glyphs -------------------------------------------------------
// Inline SVGs: aria-hidden, focusable=false. Parent carries the accessible name.

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
  // Standard Google G: four-color per Google brand guidelines; this is a
  // provider glyph, not a Mitos brand element, so the canonical colors apply.
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
// A single <style> block for auth-page-specific rules that are not in base.css.
// Keeps the component self-contained without a separate CSS file.

const styles = `
.login-link-btn {
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
.login-link-btn:hover {
  border-color: var(--magenta);
}
.login-link-btn:focus-visible {
  outline: 2px solid var(--magenta);
  outline-offset: 2px;
  border-color: transparent;
}
@media (prefers-reduced-motion: reduce) {
  .login-link-btn {
    transition: none;
  }
}
.login-divider {
  display: flex;
  align-items: center;
  gap: var(--space-3);
  margin: var(--space-5) 0;
}
.login-divider-line {
  flex: 1;
  height: 1px;
  background: var(--hairline);
  border: none;
  margin: 0;
}
.login-divider-label {
  color: var(--ink-3);
  font-size: var(--step--1);
  text-transform: uppercase;
  letter-spacing: 0.06em;
}
.login-email-form .btn {
  width: 100%;
  min-height: 44px;
  margin-top: var(--space-2);
}
.login-email-form .btn:focus-visible {
  outline: 2px solid var(--magenta);
  outline-offset: 2px;
}
`

// ---- Login component -------------------------------------------------------

export type LoginProps = {
  /** The /signup or post-auth redirect destination; defaults to window.location ?next= */
  next?: string
}

export function Login({ next }: LoginProps) {
  const [email, setEmail] = useState('')

  // Resolve the ?next= param: prop takes precedence (testable), then the
  // live URL search string (runtime). Encode once to keep it safe in hrefs.
  const resolvedNext =
    next ??
    (typeof window !== 'undefined'
      ? new URLSearchParams(window.location.search).get('next') ?? ''
      : '')

  function connectorHref(connector: string): string {
    const params = new URLSearchParams({ connector })
    if (resolvedNext) params.set('next', resolvedNext)
    return `/auth/login?${params.toString()}`
  }

  function handleEmailSubmit(e: FormEvent<HTMLFormElement>) {
    e.preventDefault()
    const params = new URLSearchParams()
    if (email.trim()) params.set('email', email.trim())
    const dest = `/signup${params.size > 0 ? `?${params.toString()}` : ''}`
    window.location.assign(dest)
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
              Sign in
            </h1>
            <p
              style={{
                margin: 0,
                color: 'var(--ink-3)',
                fontSize: 'var(--step--1)',
                textAlign: 'center',
              }}
            >
              A computer for every agent.
            </p>
          </div>

          {/* ---- Provider links: full navigations to the Go /auth/login endpoint ---- */}
          <div
            style={{
              display: 'flex',
              flexDirection: 'column',
              gap: 'var(--space-3)',
            }}
          >
            <a
              href={connectorHref('github')}
              className="login-link-btn"
              aria-label="Continue with GitHub"
            >
              <GitHubGlyph />
              Continue with GitHub
            </a>

            <a
              href={connectorHref('google')}
              className="login-link-btn"
              aria-label="Continue with Google"
            >
              <GoogleGlyph />
              Continue with Google
            </a>
          </div>

          {/* ---- Hairline divider ---- */}
          <div className="login-divider" role="separator" aria-hidden>
            <hr className="login-divider-line" />
            <span className="login-divider-label">or</span>
            <hr className="login-divider-line" />
          </div>

          {/* ---- Email to signup flow ---- */}
          <form
            className="login-email-form"
            onSubmit={handleEmailSubmit}
            aria-label="Continue with email"
          >
            <div className="form-row">
              <label htmlFor="login-email">Email</label>
              <input
                id="login-email"
                type="email"
                autoComplete="email"
                autoFocus
                placeholder="you@example.com"
                value={email}
                onChange={(e) => setEmail(e.target.value)}
              />
            </div>
            <Button variant="ghost" type="submit">
              Sign up with email
            </Button>
          </form>
        </Card>
      </div>
    </>
  )
}
