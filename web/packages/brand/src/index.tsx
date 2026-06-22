// @mitos/brand — the Fluorescence design system as a versioned package shared by
// the marketing site and the console SPA. Tokens are the single source of truth
// in tokens.css; this entrypoint re-exports the CSS and a few React primitives so
// the console renders on-brand without redefining anything.
//
// Import the styles once at the app root: `import '@mitos/brand/base.css'`.
import type { CSSProperties, ReactNode } from 'react'

export type ButtonProps = {
  children: ReactNode
  variant?: 'primary' | 'ghost'
  onClick?: () => void
  type?: 'button' | 'submit'
  disabled?: boolean
}

/** Imperative-labelled button. Primary = magenta emission; ghost = hairline. */
export function Button({ children, variant = 'primary', onClick, type = 'button', disabled }: ButtonProps) {
  return (
    <button className={`btn btn-${variant}`} onClick={onClick} type={type} disabled={disabled}>
      {children}
    </button>
  )
}

/** Raised surface: elevation by the lightness ladder + one hairline. */
export function Card({ children, style }: { children: ReactNode; style?: CSSProperties }) {
  return (
    <div className="card" style={style}>
      {children}
    </div>
  )
}

/** Terminal block with a dotted bar; use t-fork / t-ok / t-dim spans for output. */
export function Terminal({ children }: { children: ReactNode }) {
  return (
    <div className="terminal">
      <div className="terminal-bar">
        <span />
        <span />
        <span />
      </div>
      <div className="terminal-body">{children}</div>
    </div>
  )
}

export type Entity = 'ready' | 'fork' | 'parent' | 'warn'

/** Status dot whose color encodes a real entity (alive=green, fork=magenta…). */
export function StatusDot({ entity }: { entity: Entity }) {
  return <span className={`dot dot-${entity}`} aria-label={entity} />
}

/**
 * Division — the brand signature: a magenta dividing membrane around a
 * cyan-white genome core. The single most on-brand motif; the console uses it
 * for the fork-tree centerpiece. Additive, white-cored emission via screen
 * blend; never a flat neon ring.
 */
export function Division({ size = 48 }: { size?: number }) {
  return (
    <svg width={size} height={size} viewBox="0 0 48 48" fill="none" aria-hidden style={{ mixBlendMode: 'screen' }}>
      <circle cx="24" cy="24" r="21" stroke="var(--magenta)" strokeWidth="1.5" opacity="0.9" />
      <circle cx="24" cy="24" r="21" stroke="var(--signal-core)" strokeWidth="0.5" opacity="0.6" />
      <circle cx="24" cy="24" r="7" fill="var(--cyan)" opacity="0.85" />
      <circle cx="24" cy="24" r="3" fill="var(--signal-core)" />
    </svg>
  )
}
