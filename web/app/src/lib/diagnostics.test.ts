// collectDiagnostics is the ONLY thing FeedbackButton attaches to a feedback
// submission. It must be genuinely useful for debugging (version, edition,
// route, browser, viewport, theme) and it must NEVER leak anything sensitive:
// no API keys, tokens, secrets, or other users' emails. The safety property is
// enforced by construction (an explicit allowlist of fields, never a spread of
// the caps object), and this test proves it against an adversarial caps value.
import { describe, it, expect, afterEach, vi } from 'vitest'
import { collectDiagnostics } from './diagnostics'
import type { Capabilities } from '../api'
import { setAppearance, getAppearance } from '../appearance'

const baseCaps: Capabilities = {
  edition: 'hosted',
  billing: true,
  signup: false,
  teams: true,
  idp: 'oidc',
  orgSwitcher: true,
  secrets: { providers: ['kube'] },
  proof: true,
  ownership: 'hosted',
  plan: 'team',
  version: '1.6.0',
}

describe('collectDiagnostics', () => {
  const original = getAppearance()
  afterEach(() => {
    setAppearance(original)
  })

  it('contains the version, edition, and route', () => {
    const out = collectDiagnostics(baseCaps, '/billing')
    expect(out).toContain('1.6.0')
    expect(out).toContain('hosted')
    expect(out).toContain('/billing')
  })

  it('contains the plan, browser UA, viewport, theme, and a UTC timestamp', () => {
    setAppearance({ ...original, theme: 'dark' })
    const out = collectDiagnostics(baseCaps, '/sandboxes')
    expect(out).toContain('team')
    expect(out).toContain(navigator.userAgent)
    expect(out).toContain(`${window.innerWidth}x${window.innerHeight}`)
    expect(out).toContain('dark')
    // A UTC ISO-8601 timestamp string (toISOString() always ends in Z).
    expect(out).toMatch(/\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{3}Z/)
  })

  it('falls back gracefully when version/plan are absent (older server)', () => {
    const minimalCaps: Capabilities = {
      edition: 'community',
      billing: false,
      signup: false,
      teams: true,
      idp: 'oidc',
      orgSwitcher: false,
      secrets: { providers: ['kube'] },
      proof: true,
      ownership: 'self-hosted',
    }
    const out = collectDiagnostics(minimalCaps, '/')
    expect(out).toContain('community')
    expect(() => out).not.toThrow()
  })

  it('includes an org id ONLY when the capabilities doc carries one', () => {
    const withOrg = { ...baseCaps, orgId: 'org_abc123' } as Capabilities & { orgId?: string }
    expect(collectDiagnostics(withOrg, '/')).toContain('org_abc123')
    expect(collectDiagnostics(baseCaps, '/')).not.toContain('org_abc123')
  })

  it('NEVER leaks keys, tokens, secrets, bearer values, or other users emails from an adversarial caps object', () => {
    // A malicious/misconfigured server response could carry extra fields;
    // collectDiagnostics must pick only its known-safe allowlist and never
    // spread the whole object into the report.
    const adversarial = {
      ...baseCaps,
      apiKey: 'sk-live-abcdef123456',
      secretToken: 'bearer eyJhbGciOi.secret.token',
      memberEmails: ['someone-else@example.com'],
      authorizationHeader: 'Bearer super-secret-value',
    } as unknown as Capabilities

    const out = collectDiagnostics(adversarial, '/settings')
    expect(out).not.toMatch(/key|token|secret|bearer/i)
    expect(out).not.toContain('someone-else@example.com')
  })

  it('never throws even when navigator/window pieces are unusual', () => {
    const spy = vi.spyOn(window, 'innerWidth', 'get').mockReturnValue(0)
    expect(() => collectDiagnostics(baseCaps, '/')).not.toThrow()
    spy.mockRestore()
  })
})
