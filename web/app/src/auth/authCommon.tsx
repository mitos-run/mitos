// authCommon.tsx: shared auth-page UI. Imported by Login.tsx and Signup.tsx.
// Keeps glyphs, layout shell, provider buttons, shared styles, and URL utilities
// in a single place so each page contains only its page-specific bits.
//
// Brand: Fluorescence tokens only; no hardcoded hex. Mark (glow) + wordmark.
// A11y: aria-hidden glyphs, magenta focus ring, keyboard-operable targets.

import type { ReactNode } from 'react'
import { Mark, Card } from '@mitos/brand'

// ---- Provider glyphs -------------------------------------------------------
// Inline SVGs: aria-hidden, focusable=false. Parent carries the accessible name.

export function GitHubGlyph() {
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

export function GoogleGlyph() {
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

// ---- URL utilities ---------------------------------------------------------

/** Resolve the ?next= redirect destination. Prop takes precedence (testable);
 *  falls back to the live URL search string at runtime. */
export function resolveNext(nextProp?: string): string {
  return (
    nextProp ??
    (typeof window !== 'undefined'
      ? new URLSearchParams(window.location.search).get('next') ?? ''
      : '')
  )
}

/** Build the provider connector href, appending ?next= when present. */
export function connectorHref(connector: string, next: string): string {
  const params = new URLSearchParams({ connector })
  if (next) params.set('next', next)
  return `/auth/login?${params.toString()}`
}

// ---- Shared styles ---------------------------------------------------------
// Injected once by AuthShell. Login and Signup add only their page-specific rules.

export const authCommonStyles = `
.auth-link-btn {
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
.auth-link-btn:hover {
  border-color: var(--magenta);
}
.auth-link-btn:focus-visible {
  outline: 2px solid var(--magenta);
  outline-offset: 2px;
  border-color: transparent;
}
@media (prefers-reduced-motion: reduce) {
  .auth-link-btn {
    transition: none;
  }
}
.auth-divider {
  display: flex;
  align-items: center;
  gap: var(--space-3);
  margin: var(--space-5) 0;
}
.auth-divider-line {
  flex: 1;
  height: 1px;
  background: var(--hairline);
  border: none;
  margin: 0;
}
.auth-divider-label {
  color: var(--ink-3);
  font-size: var(--step--1);
  text-transform: uppercase;
  letter-spacing: 0.06em;
}
/* base.css does not define a .btn:focus-visible ring; this is the sole focus
   surface for auth submit buttons pending a brand-level ring. */
.btn:focus-visible {
  outline: 2px solid var(--magenta);
  outline-offset: 2px;
}
`

// ---- ProviderButtons --------------------------------------------------------
// The two full-navigation <a> links (GitHub first, then Google) plus the
// hairline "or" divider. Uses shared .auth-link-btn / .auth-divider classes.

export type ProviderButtonsProps = {
  next: string
}

export function ProviderButtons({ next }: ProviderButtonsProps) {
  return (
    <>
      <div
        style={{
          display: 'flex',
          flexDirection: 'column',
          gap: 'var(--space-3)',
        }}
      >
        <a href={connectorHref('github', next)} className="auth-link-btn">
          <GitHubGlyph />
          Continue with GitHub
        </a>
        <a href={connectorHref('google', next)} className="auth-link-btn">
          <GoogleGlyph />
          Continue with Google
        </a>
      </div>
      <div className="auth-divider" aria-hidden>
        <hr className="auth-divider-line" />
        <span className="auth-divider-label">or</span>
        <hr className="auth-divider-line" />
      </div>
    </>
  )
}

// ---- AuthShell --------------------------------------------------------------
// Centered Card-on-field layout with the Mark (glow) + "mitos" wordmark and
// the page title/subtitle. Wraps page-specific content via `children`.

export type AuthShellProps = {
  title: string
  subtitle: string
  children: ReactNode
}

export function AuthShell({ title, subtitle, children }: AuthShellProps) {
  return (
    <>
      <style>{authCommonStyles}</style>
      <div style={{ width: '100%', maxWidth: '400px' }}>
        <Card>
          {/* Brand mark + wordmark */}
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
              {title}
            </h1>
            <p
              style={{
                margin: 0,
                color: 'var(--ink-3)',
                fontSize: 'var(--step--1)',
                textAlign: 'center',
              }}
            >
              {subtitle}
            </p>
          </div>

          {children}
        </Card>
      </div>
    </>
  )
}
