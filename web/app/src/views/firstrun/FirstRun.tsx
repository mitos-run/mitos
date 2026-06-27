// FirstRun.tsx: guided first-run card shown on the Overview when the org has
// no measured activity. Intent-shaped by the ?uc query param; defaults to the
// generic guide when the param is absent or unrecognised.
//
// Brand: Fluorescence tokens only; no hardcoded hex. Card from @mitos/brand.
// Copy button mirrors the Verify page: Clipboard API, aria-live announcement,
// failure fallback message, magenta focus ring, 44px target.
// A11y: real heading (h2 inside the card), aria-live region for copy confirm,
//   keyboard operable, prefers-reduced-motion respected. No em/en dashes.

import { useState } from 'react'
import { Link } from '@tanstack/react-router'
import { Card } from '@mitos/brand'
import { useBilling } from '../../data/account'
import { getFirstRun } from './content'
import type { Instruments, SandboxView } from '../../api'

// ---- Page-specific styles ---------------------------------------------------

const styles = `
.firstrun-heading {
  font-size: var(--step-2);
  font-weight: 400;
  letter-spacing: var(--track-display);
  line-height: var(--lh-tight);
  margin: 0 0 var(--space-3);
  color: var(--ink);
}
.firstrun-lede {
  margin: 0 0 var(--space-5);
  font-size: var(--step-0);
  color: var(--ink-2);
  line-height: var(--lh-base);
}
.firstrun-snippet-block {
  background: var(--field-1, var(--field));
  border: 1px solid var(--hairline);
  border-radius: var(--r-md);
  padding: var(--space-4);
  margin: 0 0 var(--space-5);
}
.firstrun-snippet-label {
  margin: 0 0 var(--space-2);
  font-size: var(--step--1);
  color: var(--ink-3);
  font-weight: 600;
  text-transform: uppercase;
  letter-spacing: 0.06em;
}
.firstrun-snippet-code {
  font-family: var(--mono);
  font-size: var(--step--1);
  color: var(--cyan);
  white-space: pre;
  overflow-x: auto;
  margin: 0 0 var(--space-3);
  display: block;
}
.firstrun-copy-btn {
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
.firstrun-copy-btn:hover {
  border-color: var(--magenta);
}
.firstrun-copy-btn:focus-visible {
  outline: 2px solid var(--magenta);
  outline-offset: 2px;
  border-color: transparent;
}
@media (prefers-reduced-motion: reduce) {
  .firstrun-copy-btn { transition: none; }
}
.firstrun-billing-line {
  margin: 0 0 var(--space-5);
  font-size: var(--step--1);
  color: var(--ink-3);
  line-height: var(--lh-base);
}
.firstrun-billing-accent {
  color: var(--cyan);
  font-family: var(--mono);
  font-variant-numeric: tabular-nums;
}
.firstrun-watch-line {
  font-size: var(--step--1);
  color: var(--ink-3);
  line-height: var(--lh-base);
  margin: 0;
}
.firstrun-watch-link {
  color: var(--magenta);
  text-decoration: none;
}
.firstrun-watch-link:hover {
  text-decoration: underline;
}
.firstrun-watch-link:focus-visible {
  outline: 2px solid var(--magenta);
  outline-offset: 2px;
  border-radius: 2px;
}
`

// ---- isFirstRun predicate ---------------------------------------------------

/**
 * Returns true when the org has no measured activity: forks_served is 0 or
 * absent AND there are no Running-phase sandboxes. Treats all undefined/null
 * inputs as the zero state so the card shows while data is still loading.
 */
export function isFirstRun(
  instruments?: Instruments | null,
  sandboxes?: SandboxView[] | null,
): boolean {
  const forksServed = instruments?.forks_served ?? 0
  const hasLive = (sandboxes ?? []).some((s) => s.phase === 'Running')
  return forksServed === 0 && !hasLive
}

// ---- Local helper -----------------------------------------------------------

function fmtDollars(cents: number): string {
  return `$${(cents / 100).toFixed(2)}`
}

// ---- FirstRun component -----------------------------------------------------

export type FirstRunProps = {
  /**
   * Use-case slug from the ?uc query param. Accepting it as a prop makes the
   * component testable without touching window.location.search.
   */
  uc?: string
}

export function FirstRun({ uc }: FirstRunProps) {
  const content = getFirstRun(uc)
  const { data: billing, isLoading: billingLoading } = useBilling()

  const [copied, setCopied] = useState(false)
  const [copyFailed, setCopyFailed] = useState(false)

  async function handleCopy() {
    if (!navigator.clipboard) {
      setCopyFailed(true)
      setTimeout(() => setCopyFailed(false), 3000)
      return
    }
    try {
      await navigator.clipboard.writeText(content.snippet)
      setCopied(true)
      setTimeout(() => setCopied(false), 2000)
    } catch {
      setCopyFailed(true)
      setTimeout(() => setCopyFailed(false), 3000)
    }
  }

  return (
    <>
      <style>{styles}</style>
      <Card>
        <h2 className="firstrun-heading">{content.title}</h2>
        <p className="firstrun-lede">{content.lede}</p>

        {/* Snippet block with copy button */}
        <div className="firstrun-snippet-block" role="region" aria-label="Runnable snippet">
          <p className="firstrun-snippet-label">Python</p>
          <code className="firstrun-snippet-code">{content.snippet}</code>
          <button
            type="button"
            className="firstrun-copy-btn"
            onClick={() => void handleCopy()}
            aria-label={
              copied ? 'Copied to clipboard' : 'Copy snippet to clipboard'
            }
          >
            {copyFailed
              ? 'Copy failed, select the text'
              : copied
                ? 'Copied'
                : 'Copy snippet'}
          </button>
          {copyFailed && (
            <p
              style={{
                margin: 'var(--space-2) 0 0',
                fontSize: 'var(--step--1)',
                color: 'var(--ink-3)',
              }}
            >
              Clipboard unavailable. Select the snippet above.
            </p>
          )}
        </div>

        {/* Visually hidden assertive region for the copy announcement.
            Announced immediately without re-reading the snippet block. */}
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
          {copied ? 'Snippet copied to clipboard' : ''}
        </div>

        {/* Free credit + spend line */}
        {billing ? (
          <p className="firstrun-billing-line">
            You have{' '}
            <span className="firstrun-billing-accent">
              {fmtDollars(billing.balance_cents)}
            </span>{' '}
            in free credit. Spent so far:{' '}
            <span className="firstrun-billing-accent">
              {fmtDollars(billing.spend_cents)}
            </span>
            . Watch it grow as you fork.
          </p>
        ) : billingLoading ? null : (
          <p className="firstrun-billing-line">
            Free credit available. Run your first fork to see your spend here.
          </p>
        )}

        {/* Fork tree / live canvas pointer */}
        <p className="firstrun-watch-line">
          {content.watchFor}{' '}
          <Link to="/forks" className="firstrun-watch-link">
            Open the fork tree.
          </Link>
        </p>
      </Card>
    </>
  )
}
