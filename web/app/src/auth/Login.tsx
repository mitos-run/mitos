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
import { Button } from '@mitos/brand'
import { AuthShell, ProviderButtons, resolveNext } from './authCommon'

// ---- Page-specific styles --------------------------------------------------

const styles = `
.login-email-form .btn {
  width: 100%;
  min-height: 44px;
  margin-top: var(--space-2);
}
`

// ---- Login component -------------------------------------------------------

export type LoginProps = {
  /** The /signup or post-auth redirect destination; defaults to window.location ?next= */
  next?: string
}

export function Login({ next }: LoginProps) {
  const [email, setEmail] = useState('')
  const resolvedNext = resolveNext(next)

  function handleEmailSubmit(e: FormEvent<HTMLFormElement>) {
    e.preventDefault()
    const params = new URLSearchParams()
    if (email.trim()) params.set('email', email.trim())
    if (resolvedNext) params.set('next', resolvedNext)
    const dest = `/signup${params.size > 0 ? `?${params.toString()}` : ''}`
    window.location.assign(dest)
  }

  return (
    <AuthShell title="Sign in" subtitle="A computer for every agent.">
      <style>{styles}</style>

      {/* Provider links: full navigations to the Go /auth/login endpoint */}
      <ProviderButtons next={resolvedNext} />

      {/* Email to signup flow */}
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
            name="email"
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
    </AuthShell>
  )
}
