# Console B3a: Trust and compliance surface Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development. Steps use checkbox (`- [ ]`). Do NOT invoke finishing-a-development-branch; implement, test, commit, report.

**Goal:** A Trust and compliance view that renders the project's HONEST security posture: the no-external-security-review banner (the #194 gate), the threat-model and SECURITY links, the self-host data-residency posture, and the capability-gated hosted compliance artifacts framed honestly (requested / in-progress, never claimed-as-done).

**Architecture:** Frontend-only on the merged console. No new BFF endpoint: the content is static, honest copy plus capability-driven branches (self-host vs hosted via `caps.ownership`). The integrity discipline is load-bearing here: this view must not fabricate a single compliance or security claim.

**Tech Stack:** React+Vite+TS, TanStack Query (caps), `@mitos/brand`, Vitest + vitest-axe.

**Scope note:** B3a of the B3 enterprise split. B3b (audit retention/export), B3c (data-retention), B3d (custom roles + per-project RBAC), B3e (SAML SSO), B3f (SCIM) follow. The auth-critical B3e/B3f get threat-model deltas + named-human review.

## Global Constraints

- **Punctuation (strict):** no em (U+2014) or en (U+2013) dashes anywhere. Only `.` `,` `;` `:`; ASCII `-`. Verify each commit.
- **Commits:** conventional + DCO (`git commit -s`). **Staging:** explicit paths only.
- **Integrity (LOAD-BEARING):** render ONLY honest, accurate security/compliance statements. The OSS view states plainly that NO external security review has happened yet and links the threat model + SECURITY.md. The hosted compliance section frames SOC2 / ISO / HIPAA-BAA / DPA as "available on request" or "in progress", NEVER as completed/certified (none are). No fabricated certification, badge, or claim. This mirrors the README and `docs/compliance-claims.md` honesty rules.
- **Capability-gated:** the hosted compliance section renders only when `caps.ownership === 'hosted'`; the self-host residency section when `caps.ownership === 'self-hosted'`. Both editions show the honesty banner.
- **Responsive + accessible (spec 4.6):** the view is responsive; links have discernible names; axe zero violations.
- **TypeScript strict** clean; SPA suite exits 0.

## File Structure

- `web/app/src/views/Trust.tsx` (create) - the Trust and compliance view.
- `web/app/src/nav/routes.tsx` (modify) - add `/trust` route in the Govern group.
- `web/packages/brand/src/base.css` (modify) - trust-banner + posture styles.
- Tests alongside.

---

### Task 1: Trust and compliance view

**Files:**
- Create: `web/app/src/views/Trust.tsx`
- Modify: `web/app/src/nav/routes.tsx`
- Test: `web/app/src/views/Trust.test.tsx`

**Interfaces:**
- Consumes: `useCapabilities` (for `ownership`), `@mitos/brand` primitives.
- Produces: the `Trust` view; a `/trust` route (group Govern, label "Trust").

The view renders, in order:
1. **Security review banner (always):** an amber-accented banner stating "No external security review has been performed yet." with one sentence that the threat model documents the exact per-boundary status, and two links: "Threat model" -> `https://github.com/mitos-run/mitos/blob/main/docs/threat-model.md` and "Report a vulnerability" -> `https://github.com/mitos-run/mitos/blob/main/SECURITY.md`. (External links open in a new tab with `rel="noopener noreferrer"`.)
2. **Self-host posture (when `ownership === 'self-hosted'`):** "Running on your infrastructure" with a line that data never leaves the operator's cluster, and that the operator controls residency, retention, and the IdP. A link to the self-host security checklist (`docs/security/` or the install docs).
3. **Hosted compliance (when `ownership === 'hosted'`):** a section titled "Compliance" listing SOC2 Type II, ISO 27001, HIPAA + BAA, DPA, and a sub-processor list, EACH labelled honestly with a status like "Available on request" or "In progress" (NOT "Certified"/"Complete"). A "Request compliance documentation" contact affordance. A short honest note that these are operated/delivered artifacts.

- [ ] **Step 1: Write the failing test `web/app/src/views/Trust.test.tsx`**

```tsx
import { describe, it, expect, vi, beforeEach } from 'vitest'
import { waitFor, screen } from '@testing-library/react'
import { renderAt } from '../test/utils'
import type { Capabilities } from '../api'

function caps(ownership: 'self-hosted' | 'hosted'): Capabilities {
  return { edition: ownership === 'hosted' ? 'hosted' : 'community', billing: ownership === 'hosted', signup: false, teams: true, idp: 'oidc', orgSwitcher: ownership === 'hosted', secrets: { providers: ['kube'] }, proof: true, ownership }
}

function mockCaps(c: Capabilities) {
  vi.spyOn(globalThis, 'fetch').mockImplementation((input) => {
    const url = String(input)
    if (url.endsWith('/console/capabilities')) return Promise.resolve(new Response(JSON.stringify(c), { status: 200, headers: { 'content-type': 'application/json' } }))
    return Promise.resolve(new Response(JSON.stringify({}), { status: 200, headers: { 'content-type': 'application/json' } }))
  })
}

describe('Trust view', () => {
  beforeEach(() => vi.restoreAllMocks())

  it('always shows the honest no-external-review banner and the threat-model link', async () => {
    mockCaps(caps('self-hosted'))
    await renderAt('/trust', caps('self-hosted'))
    await waitFor(() => expect(screen.getByText(/no external security review/i)).toBeInTheDocument())
    expect(screen.getByRole('link', { name: /threat model/i })).toHaveAttribute('href', expect.stringContaining('threat-model.md'))
  })

  it('shows the self-host residency posture for self-hosted', async () => {
    mockCaps(caps('self-hosted'))
    await renderAt('/trust', caps('self-hosted'))
    await waitFor(() => expect(screen.getByText(/your infrastructure/i)).toBeInTheDocument())
  })

  it('shows hosted compliance framed honestly (not certified) for hosted', async () => {
    mockCaps(caps('hosted'))
    await renderAt('/trust', caps('hosted'))
    await waitFor(() => expect(screen.getByText(/SOC2/i)).toBeInTheDocument())
    // honest framing: no "certified"/"compliant" claim text
    expect(screen.queryByText(/\bcertified\b/i)).not.toBeInTheDocument()
  })
})
```

- [ ] **Step 2: Run it, confirm it fails**

Run: `pnpm -C web/app test src/views/Trust.test.tsx`
Expected: FAIL (no `/trust` route, no component).

- [ ] **Step 3: Implement `web/app/src/views/Trust.tsx`** per the three-section spec above, reading `ownership` from `useCapabilities`. Use honest copy only; external links carry `target="_blank" rel="noopener noreferrer"`. The hosted compliance items render with an explicit honest status word ("Available on request" / "In progress"); never "Certified" or "Compliant".

- [ ] **Step 4: Add the `/trust` route in `web/app/src/nav/routes.tsx`** (group Govern, label "Trust", import the view). It is available in both editions (no `when` gate; the content branches internally on ownership).

- [ ] **Step 5: Run the test, full suite, typecheck**

Run: `pnpm -C web/app test src/views/Trust.test.tsx && pnpm -C web/app test && pnpm -C web/app typecheck`
Expected: PASS, clean.

- [ ] **Step 6: Commit**

```bash
git add web/app/src/views/Trust.tsx web/app/src/nav/routes.tsx web/app/src/views/Trust.test.tsx
git commit -s -m "feat(console): Trust and compliance view with honest security posture"
```

---

### Task 2: Styles, a11y, final verification

**Files:**
- Modify: `web/packages/brand/src/base.css`
- Create: `web/app/src/views/Trust.a11y.test.tsx`

- [ ] **Step 1: Append token-driven styles** for the trust banner (`.trust-banner` amber-accented via `var(--amber)`), the posture sections, and a `.compliance-item` status pill (reuse `.badge`). No raw hex. Mobile rule.

- [ ] **Step 2: Write the axe a11y test `web/app/src/views/Trust.a11y.test.tsx`** (render `/trust` for both ownerships, assert zero axe violations; links have discernible names). Fix any real violation.

- [ ] **Step 3: Final verification**

Run: `pnpm -C web/app test` (exit 0) ; `pnpm -C web/app typecheck` (clean) ; `pnpm -C web/app build` (succeeds)
Run: `grep -rnP '\xe2\x80\x94|\xe2\x80\x93' web/app/src/views/Trust.tsx web/app/src/views/Trust.test.tsx web/packages/brand/src/base.css` (empty)

- [ ] **Step 4: Commit**

```bash
git add web/packages/brand/src/base.css web/app/src/views/Trust.a11y.test.tsx
git commit -s -m "feat(console): Trust view styles and accessibility checks"
```

---

## Self-Review

**Spec coverage (section 5.6 Trust surface):** the honesty banner (#194 gate), threat-model + SECURITY links, self-host residency posture, and hosted compliance artifacts framed honestly. Covered.

**Integrity:** the view makes no fabricated compliance/security claim; the OSS banner states no external review has happened; hosted artifacts are "available on request"/"in progress", never certified. The test asserts the absence of a "certified" claim.

**No new attack surface:** frontend-only, no new endpoint, no auth change. No threat-model delta required for B3a (unlike B3e/B3f).
