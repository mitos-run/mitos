// Verify.tsx: magic-link verification page. Exchanges ?token= for an account
// confirmation, shows the provisioned API key exactly once, and guides the user
// to the console. Never logs the token or the key value.
//
// Brand: Fluorescence tokens only; no hardcoded hex. AuthShell for chrome.
// Layout: same centered Card-on-field shell as Login/Signup for visual parity.
// A11y: single aria-live region drives all state transitions; a separate
//   assertive region announces the copy confirmation without re-reading the key;
//   copy button is a real <button> with a focus ring; 44px tap targets;
//   keyboard operable; prefers-reduced-motion respected.

import { useState, useEffect, useRef } from 'react'
import { post } from '../api'
import { AuthShell } from './authCommon'

// ---- Page-specific styles ----------------------------------------------------

const styles = `
.verify-key-block {
  background: var(--field-1, var(--field));
  border: 1px solid var(--hairline);
  border-radius: var(--r-md);
  padding: var(--space-4);
  margin: var(--space-4) 0;
}
.verify-key-label {
  margin: 0 0 var(--space-2);
  font-size: var(--step--1);
  color: var(--ink-3);
  font-weight: 600;
  text-transform: uppercase;
  letter-spacing: 0.06em;
}
.verify-key-value {
  font-family: var(--mono);
  font-size: var(--step-0);
  font-variant-numeric: tabular-nums;
  color: var(--magenta);
  word-break: break-all;
  margin: 0 0 var(--space-3);
  user-select: all;
}
.verify-copy-btn {
  display: inline-flex;
  align-items: center;
  gap: var(--space-2);
  min-height: 44px;
  padding: var(--space-2) var(--space-4);
  border-radius: var(--r-md);
  font: inherit;
  font-size: var(--step--1);
  background: transparent;
  color: var(--ink);
  border: 1px solid var(--hairline);
  cursor: pointer;
  transition: border-color var(--dur) var(--ease);
}
.verify-copy-btn:hover {
  border-color: var(--magenta);
}
.verify-copy-btn:focus-visible {
  outline: 2px solid var(--magenta);
  outline-offset: 2px;
  border-color: transparent;
}
@media (prefers-reduced-motion: reduce) {
  .verify-copy-btn { transition: none; }
}
.verify-warning {
  margin: var(--space-3) 0 0;
  font-size: var(--step--1);
  color: var(--ink-3);
  line-height: var(--lh-base);
}
.verify-snippet {
  display: block;
  font-family: var(--mono);
  font-size: var(--step--1);
  background: var(--field-1, var(--field));
  border: 1px solid var(--hairline);
  border-radius: var(--r-sm);
  padding: var(--space-2) var(--space-3);
  margin: var(--space-2) 0 var(--space-5);
  color: var(--ink);
}
.verify-continue-link {
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
  cursor: pointer;
  transition: box-shadow var(--dur) var(--ease);
}
.verify-continue-link:hover {
  box-shadow: 0 0 0 1px var(--magenta), 0 0 16px color-mix(in srgb, var(--magenta) 50%, transparent);
}
.verify-continue-link:focus-visible {
  outline: 2px solid var(--magenta);
  outline-offset: 2px;
}
@media (prefers-reduced-motion: reduce) {
  .verify-continue-link { transition: none; }
}
.verify-error-link {
  color: var(--magenta);
  text-decoration: none;
}
.verify-error-link:hover {
  text-decoration: underline;
}
.verify-error-link:focus-visible {
  outline: 2px solid var(--magenta);
  outline-offset: 2px;
  border-radius: 2px;
}
`

// ---- Types ------------------------------------------------------------------

type VerifyResponse = {
  accountId: string
  orgId: string
  email: string
  alreadyDone: boolean
  apiKey?: string
  apiKeyId?: string
  /** Use-case slug carried from signup (e.g. "ai-coding"). Empty string when absent. */
  useCase?: string
}

type VerifyState =
  | { kind: 'loading' }
  | { kind: 'success-with-key'; email: string; apiKey: string; useCase?: string }
  | { kind: 'success-already-done'; email: string; useCase?: string }
  | { kind: 'error' }
  | { kind: 'no-token' }

// ---- Verify component --------------------------------------------------------

export type VerifyProps = {
  /** Token from the magic link. Falls back to ?token= in the URL at runtime. */
  token?: string
}

export function Verify({ token: tokenProp }: VerifyProps) {
  const token =
    tokenProp ??
    (typeof window !== 'undefined'
      ? new URLSearchParams(window.location.search).get('token') ?? ''
      : '')

  const [state, setState] = useState<VerifyState>(
    token ? { kind: 'loading' } : { kind: 'no-token' },
  )
  const [copied, setCopied] = useState(false)
  const [copyFailed, setCopyFailed] = useState(false)

  const didPost = useRef(false)
  useEffect(() => {
    if (!token || didPost.current) return
    didPost.current = true
    let cancelled = false
    post<VerifyResponse>('/onboarding/verify', { token })
      .then((res) => {
        if (cancelled || !res) return
        if (res.alreadyDone || !res.apiKey) {
          setState({ kind: 'success-already-done', email: res.email, useCase: res.useCase })
        } else {
          // Stash the one-time first key so the console first-run can display
          // the masked export snippet without a second API round-trip. The raw
          // key is a secret: it is written only to sessionStorage here and is
          // NEVER logged or put in any error message.
          sessionStorage.setItem('mitos.firstKey', res.apiKey)
          setState({ kind: 'success-with-key', email: res.email, apiKey: res.apiKey, useCase: res.useCase })
        }
      })
      .catch(() => {
        if (!cancelled) setState({ kind: 'error' })
      })
    return () => {
      cancelled = true
    }
  }, [token])

  async function handleCopy(key: string) {
    if (!navigator.clipboard) {
      setCopyFailed(true)
      setTimeout(() => setCopyFailed(false), 3000)
      return
    }
    try {
      await navigator.clipboard.writeText(key)
      setCopied(true)
      setTimeout(() => setCopied(false), 2000)
    } catch {
      setCopyFailed(true)
      setTimeout(() => setCopyFailed(false), 3000)
    }
  }

  const subtitle =
    state.kind === 'loading'
      ? 'One moment while we verify your link.'
      : state.kind === 'success-with-key'
        ? 'Your account is ready. Save your key before continuing.'
        : state.kind === 'success-already-done'
          ? 'Your email is already verified.'
          : 'There was a problem with your link.'

  function renderState() {
    switch (state.kind) {
      case 'loading':
        return (
          <p
            style={{
              textAlign: 'center',
              color: 'var(--ink-3)',
              fontSize: 'var(--step--1)',
              margin: 'var(--space-4) 0',
            }}
          >
            Verifying your link...
          </p>
        )

      case 'success-with-key':
        return (
          <>
            <p
              style={{
                margin: '0 0 var(--space-3)',
                fontSize: 'var(--step-0)',
                color: 'var(--ink)',
                textAlign: 'center',
                lineHeight: 'var(--lh-base)',
              }}
            >
              Email confirmed: <strong>{state.email}</strong>
            </p>

            <div
              className="verify-key-block"
              role="region"
              aria-label="Your API key"
            >
              <p className="verify-key-label">API key</p>
              <p className="verify-key-value">{state.apiKey}</p>
              <button
                type="button"
                className="verify-copy-btn"
                onClick={() => void handleCopy(state.apiKey)}
                aria-label={copied ? 'Copied to clipboard' : 'Copy API key to clipboard'}
              >
                {copyFailed ? 'Copy failed, select the key' : copied ? 'Copied' : 'Copy key'}
              </button>
              {copyFailed && (
                <p
                  style={{
                    margin: 'var(--space-2) 0 0',
                    fontSize: 'var(--step--1)',
                    color: 'var(--ink-3)',
                  }}
                >
                  Clipboard unavailable. Select the key above.
                </p>
              )}
              <p className="verify-warning">
                Save this key now. For your security it is shown only once.
              </p>
            </div>

            <p
              style={{
                margin: 'var(--space-2) 0 var(--space-1)',
                fontSize: 'var(--step--1)',
                color: 'var(--ink-3)',
              }}
            >
              Install the SDK:
            </p>
            <code className="verify-snippet">pip install mitos-run</code>

            <a href={state.useCase ? `/?uc=${state.useCase}` : '/'} className="verify-continue-link">
              Continue to console
            </a>
          </>
        )

      case 'success-already-done':
        return (
          <>
            <p
              style={{
                margin: '0 0 var(--space-5)',
                fontSize: 'var(--step-0)',
                color: 'var(--ink)',
                textAlign: 'center',
                lineHeight: 'var(--lh-base)',
              }}
            >
              You are already verified.
            </p>
            <a href={state.useCase ? `/?uc=${state.useCase}` : '/'} className="verify-continue-link">
              Continue to console
            </a>
          </>
        )

      case 'error':
      case 'no-token':
        return (
          <>
            <p
              style={{
                margin: '0 0 var(--space-3)',
                fontSize: 'var(--step-0)',
                color: 'var(--ink)',
                textAlign: 'center',
                lineHeight: 'var(--lh-base)',
              }}
            >
              This link is invalid or has expired.
            </p>
            <p
              style={{
                textAlign: 'center',
                fontSize: 'var(--step--1)',
                color: 'var(--ink-3)',
                margin: 0,
              }}
            >
              <a href="/signup" className="verify-error-link">
                Start over
              </a>{' '}
              to request a new link.
            </p>
          </>
        )
    }
  }

  return (
    <AuthShell title="Verify your email" subtitle={subtitle}>
      <style>{styles}</style>

      {/* Status region: always in the DOM so screen readers observe content
          changes rather than a region appearing. All state transitions render
          here; the region is announced without re-reading the entire card. */}
      <div aria-live="polite">
        {renderState()}
      </div>

      {/* Visually hidden assertive region for the copy confirmation.
          Announced immediately without re-reading the key block. */}
      <div
        aria-live="assertive"
        aria-atomic="true"
        style={{
          position: 'absolute',
          width: 1,
          height: 1,
          overflow: 'hidden',
          clipPath: 'inset(50%)',
          whiteSpace: 'nowrap',
        }}
      >
        {copied ? 'API key copied to clipboard' : ''}
      </div>
    </AuthShell>
  )
}
