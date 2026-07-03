# Docs key injection: design options

Status: proposed. Tracking issue: #644 (part of the #622 onboarding follow-ups).

## Problem

The strongest known docs onboarding affordance is the Stripe pattern: every code
snippet in the docs carries the signed-in user's real API key, so copy-paste is
literally runnable with zero editing. mitos.run is single-origin (the marketing
and docs pages share the origin with the console through the frontdoor), so the
console session cookie is already present on docs pages and the pattern is
reachable.

The obstacle is a deliberate security property: mitos API keys are shown once at
creation and stored hashed. The docs cannot fetch an existing key because the
server no longer has it. Any injection design must create key material, not
retrieve it.

## Constraints

- Keys are shown once; the server stores only hashes. Non-negotiable.
- Single-origin session: docs pages can call console endpoints with the session
  cookie, subject to CSRF discipline.
- Capability-gated: the affordance exists only where the deployment enables it
  (hosted on by default; self-host opt-in). Signed-out docs pages degrade to the
  current placeholder.
- Any new minting surface moves the threat model and needs the same-PR delta
  and a named human review.

## Option 1: ephemeral docs keys (recommended)

A console endpoint mints a short-lived, low-scope key for the signed-in org on
demand. Docs pages fetch it client-side and substitute the placeholder in every
snippet.

- Scope ceiling: the default sandboxes scope only; never admin or billing.
- TTL: hours, not days; auto-expiring server-side; labeled in the keys list as
  a docs key so users understand where it came from and can revoke it.
- Rate limit: per org and per session; a second request within the TTL returns
  the same key id but can never return the secret again (shown-once holds; the
  docs page keeps it in memory or sessionStorage, mirroring the first-run flow).
- CSRF: mint only on a POST with the standard same-origin checks; the endpoint
  is useless cross-origin because the response is read by page script.
- Threat-model delta: one new row, a session-cookie-reachable minting surface
  with scope and TTL ceilings; abuse case is a hijacked session minting keys,
  which is already the threat class of the existing keys page, with a lower
  ceiling here.

Why recommended: it is the only option that delivers the actual aha (paste and
run with no edits), the mechanics reuse the first-run shown-once handling, and
the new surface is strictly weaker than the existing key-creation surface.

## Option 2: placeholder plus copy affordance

Docs detect the session and render a button that deep-links to the console
key-create flow with a return-to-docs redirect; no key material ever appears on
docs pages.

- No new minting surface; no threat-model movement beyond a redirect parameter
  (which must be an allowlisted relative path to avoid open redirects).
- Materially weaker aha: the user still context-switches, creates, copies, and
  edits the snippet by hand.

Fallback if Option 1 is rejected on threat-model grounds.

## Option 3: session-bound snippet runner

Snippets execute server-side against a runner that acts with the session
identity; no key is exposed at all.

- Highest effort by far (a runner service, output streaming, per-org limits);
  overlaps with the console first-run live-execution work rather than docs.
- Out of scope for #644; noted for completeness.

## Decision requested

Approve Option 1 with the ceilings above, or direct Option 2. Implementation
follows only after the threat-model delta is reviewed by a named human per the
security practices.
