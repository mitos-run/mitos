// Builds the plain-text diagnostics block FeedbackButton attaches to every
// feedback submission (mailto: body or GitHub issue body), shown to the user
// verbatim in a read-only <details> before they send it (transparency: the
// user sees exactly what is sent, nothing hidden).
//
// SAFETY: this is an explicit ALLOWLIST of known-safe fields. It must never
// spread `caps` (or any other object that might carry a key, token, secret,
// or another user's email) into the report. Only add a new line here for a
// field that has been deliberately reviewed as safe to disclose.
import type { Capabilities } from '../api'
import { getAppearance } from '../appearance'

// DiagnosticsCapabilities lets a future server add an org id to the
// capabilities doc without collectDiagnostics needing a signature change;
// today no such field exists (see the WS9 brief: "do not add new server
// fields for this"), so orgId is simply absent and the line is omitted.
export type DiagnosticsCapabilities = Capabilities & { orgId?: string }

export function collectDiagnostics(caps: DiagnosticsCapabilities, route: string): string {
  const lines = [
    'Mitos diagnostics',
    `version: ${caps.version ?? 'unknown'}`,
    `edition: ${caps.edition}`,
    `plan: ${caps.plan ?? 'n/a'}`,
    `route: ${route}`,
    `browser: ${safeNavigatorUA()}`,
    `viewport: ${safeViewport()}`,
    `theme: ${getAppearance().theme}`,
  ]
  if (caps.orgId) {
    lines.push(`org: ${caps.orgId}`)
  }
  // toISOString() always renders UTC (trailing Z), regardless of the
  // browser's local timezone.
  lines.push(`timestamp: ${new Date().toISOString()}`)
  return lines.join('\n')
}

function safeNavigatorUA(): string {
  try {
    return typeof navigator !== 'undefined' && navigator.userAgent ? navigator.userAgent : 'unknown'
  } catch {
    return 'unknown'
  }
}

function safeViewport(): string {
  try {
    if (typeof window === 'undefined') return 'unknown'
    return `${window.innerWidth}x${window.innerHeight}`
  } catch {
    return 'unknown'
  }
}
