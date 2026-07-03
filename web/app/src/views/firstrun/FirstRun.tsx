// FirstRun.tsx: three-step guided first-run shown on the Overview to a new org.
//
// Step 1: copy your masked API key.
// Step 2: pick a runtime, copy the runnable snippet.
// Step 3: live "Waiting for your first call..." that celebrates when the first
//         exec lands.
//
// Brand: Fluorescence tokens only; no hardcoded hex. Card from @mitos/brand.
// Copy button: Clipboard API, aria-live announcement, failure fallback message,
//   magenta focus ring, 44px target. Mirrors Verify page copy pattern.
// A11y: real heading (h2 inside Card), real buttons, tablist with roving focus
//   (arrow keys), aria-live for copy + step completion, prefers-reduced-motion
//   respected. No em or en dashes.

import { useState } from 'react'
import { Link } from '@tanstack/react-router'
import { Card } from '@mitos/brand'
import { useBilling } from '../../data/account'
import { useFirstActivity } from '../../data/firstActivity'
import { getFirstRun, RUNTIMES } from './content'
import type { Runtime } from './content'
import { peekFirstKey, takeFirstKey, maskKey } from './firstKey'
import { Celebrate } from '../../ui/Celebrate'
import { Tabs } from '../../ui/Tabs'
import { fmtDollars } from '../../api'
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
/* Steps */
.firstrun-step {
  margin: 0 0 var(--space-6);
  padding: var(--space-4);
  border: 1px solid var(--hairline);
  border-radius: var(--r-md);
}
.firstrun-step[data-done="true"] {
  border-color: var(--cyan);
}
.firstrun-step-header {
  display: flex;
  align-items: center;
  gap: var(--space-3);
  margin: 0 0 var(--space-4);
}
.firstrun-step-num {
  display: inline-flex;
  align-items: center;
  justify-content: center;
  width: 24px;
  height: 24px;
  border-radius: 50%;
  font-size: var(--step--1);
  font-weight: 600;
  background: var(--field-1, var(--field));
  color: var(--ink-3);
  border: 1px solid var(--hairline);
  flex-shrink: 0;
}
.firstrun-step[data-done="true"] .firstrun-step-num {
  background: var(--cyan);
  color: var(--bg);
  border-color: transparent;
}
.firstrun-step-title {
  font-size: var(--step-0);
  font-weight: 400;
  color: var(--ink);
  margin: 0;
}
.firstrun-step-check {
  color: var(--cyan);
  font-size: var(--step-0);
  margin-left: auto;
}
/* Key block */
.firstrun-key-block {
  background: var(--field-1, var(--field));
  border: 1px solid var(--hairline);
  border-radius: var(--r-md);
  padding: var(--space-4);
  margin: 0 0 var(--space-3);
}
.firstrun-key-line {
  font-family: var(--mono);
  font-size: var(--step--1);
  color: var(--cyan);
  white-space: pre;
  overflow-x: auto;
  margin: 0 0 var(--space-3);
  display: block;
}
.firstrun-key-static {
  color: var(--ink-2);
}
/* Snippet block */
.firstrun-snippet-block {
  background: var(--field-1, var(--field));
  border: 1px solid var(--hairline);
  border-radius: var(--r-md);
  padding: var(--space-4);
  margin: 0 0 var(--space-3);
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
/* Copy button (shared between key and snippet) */
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
/* Create-key fallback: one explaining sentence, then the action as the
   primary affordance (a button-styled link mirroring .btn-primary). */
.firstrun-create-key-line {
  font-size: var(--step--1);
  color: var(--ink-3);
  margin: 0 0 var(--space-3);
  line-height: var(--lh-base);
}
.firstrun-create-key-btn {
  display: inline-flex;
  align-items: center;
  justify-content: center;
  min-height: 44px;
  padding: var(--space-2) var(--space-4);
  border-radius: var(--r-md);
  font: inherit;
  font-size: var(--step--1);
  background: var(--magenta);
  color: var(--field);
  text-decoration: none;
  transition: box-shadow var(--dur) var(--ease);
}
.firstrun-create-key-btn:hover {
  box-shadow: 0 0 0 1px var(--magenta), 0 0 16px rgba(255, 69, 200, 0.5);
}
.firstrun-create-key-btn:focus-visible {
  outline: 2px solid var(--magenta);
  outline-offset: 2px;
}
@media (prefers-reduced-motion: reduce) {
  .firstrun-create-key-btn { transition: none; }
}
/* Tab bar spacing inside step 2 */
.firstrun-tab-bar {
  margin: 0 0 var(--space-3);
}
/* Step 3: waiting + next-step links */
.firstrun-waiting {
  font-size: var(--step--1);
  color: var(--ink-3);
  margin: 0 0 var(--space-3);
  line-height: var(--lh-base);
}
@keyframes firstrun-pulse {
  0%, 100% { opacity: 1; }
  50%       { opacity: 0.5; }
}
.firstrun-waiting {
  animation: firstrun-pulse 2s ease-in-out infinite;
}
@media (prefers-reduced-motion: reduce) {
  .firstrun-waiting { animation: none; }
}
.firstrun-next-links {
  list-style: none;
  padding: 0;
  margin: var(--space-3) 0 0;
  display: flex;
  flex-wrap: wrap;
  gap: var(--space-3);
}
.firstrun-next-link {
  color: var(--magenta);
  font-size: var(--step--1);
  text-decoration: none;
}
.firstrun-next-link:hover { text-decoration: underline; }
.firstrun-next-link:focus-visible {
  outline: 2px solid var(--magenta);
  outline-offset: 2px;
  border-radius: 2px;
}
/* Visually hidden live region */
.firstrun-sr-live {
  position: absolute;
  width: 1px;
  height: 1px;
  overflow: hidden;
  clip-path: inset(50%);
  white-space: nowrap;
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

  // Read the first key exactly once at mount; never re-read after copy/clear.
  const [key] = useState(() => peekFirstKey())

  // Tab runtime selection.
  const [runtime, setRuntime] = useState<Runtime>('python')

  // Per-step done state.
  const [keyDone, setKeyDone] = useState(false)
  const [snippetDone, setSnippetDone] = useState(false)

  // Key copy feedback.
  const [keyAnnounce, setKeyAnnounce] = useState('')
  const [keyCopyFailed, setKeyCopyFailed] = useState(false)

  // Snippet copy feedback.
  const [snippetCopied, setSnippetCopied] = useState(false)
  const [snippetAnnounce, setSnippetAnnounce] = useState('')
  const [snippetCopyFailed, setSnippetCopyFailed] = useState(false)

  // Live first-activity poll: active flips true when the first exec lands.
  const activity = useFirstActivity(true)
  const active = activity.data?.active === true

  // ---- handlers -------------------------------------------------------------

  async function handleKeyCopy() {
    if (!key) return
    if (!navigator.clipboard) {
      setKeyCopyFailed(true)
      setTimeout(() => setKeyCopyFailed(false), 3000)
      return
    }
    try {
      await navigator.clipboard.writeText(`export MITOS_API_KEY=${key}`)
      takeFirstKey()
      setKeyDone(true)
      setKeyAnnounce('API key copied to clipboard')
      setTimeout(() => setKeyAnnounce(''), 2000)
    } catch {
      setKeyCopyFailed(true)
      setTimeout(() => setKeyCopyFailed(false), 3000)
    }
  }

  async function handleSnippetCopy() {
    if (!navigator.clipboard) {
      setSnippetCopyFailed(true)
      setTimeout(() => setSnippetCopyFailed(false), 3000)
      return
    }
    try {
      await navigator.clipboard.writeText(content.snippets[runtime])
      setSnippetDone(true)
      setSnippetCopied(true)
      setSnippetAnnounce('Snippet copied to clipboard')
      setTimeout(() => {
        setSnippetCopied(false)
        setSnippetAnnounce('')
      }, 2000)
    } catch {
      setSnippetCopyFailed(true)
      setTimeout(() => setSnippetCopyFailed(false), 3000)
    }
  }

  // ---- render ---------------------------------------------------------------

  return (
    <>
      <style>{styles}</style>
      <Card>
        <h2 className="firstrun-heading">{content.title}</h2>
        <p className="firstrun-lede">{content.lede}</p>

        {/* Free credit + spend line */}
        {billing ? (
          <p className="firstrun-billing-line">
            <span className="firstrun-billing-accent">
              {fmtDollars(billing.balance_cents)}
            </span>{' '}
            free credit remaining. Spent so far:{' '}
            <span className="firstrun-billing-accent">
              {fmtDollars(billing.spend_cents)}
            </span>
            . Watch your spend grow as you fork.
          </p>
        ) : billingLoading ? null : (
          <p className="firstrun-billing-line">
            Free credit available. Run your first fork to see your spend here.
          </p>
        )}

        {/* Step 1: copy your key */}
        <section
          data-step="key"
          data-done={keyDone ? 'true' : undefined}
          className="firstrun-step"
          aria-label="Step 1: copy your API key"
        >
          <div className="firstrun-step-header">
            <span className="firstrun-step-num" aria-hidden="true">1</span>
            <p className="firstrun-step-title">Copy your API key</p>
            {keyDone && (
              <span className="firstrun-step-check" aria-hidden="true">
                {'✓'}
              </span>
            )}
          </div>

          {key ? (
            <div className="firstrun-key-block">
              {/* Only the masked form is rendered; the raw tail never touches the DOM. */}
              <code className="firstrun-key-line">
                <span className="firstrun-key-static">export MITOS_API_KEY=</span>
                {maskKey(key)}
              </code>
              <button
                type="button"
                className="firstrun-copy-btn"
                onClick={() => void handleKeyCopy()}
                aria-label={keyDone ? 'API key copied' : 'Copy API key to clipboard'}
              >
                {keyCopyFailed
                  ? 'Copy failed, select the text'
                  : keyDone
                    ? 'Copied'
                    : 'Copy key'}
              </button>
              {keyCopyFailed && (
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
            </div>
          ) : (
            <div>
              <p className="firstrun-create-key-line">
                Your API key is shown only once, when it is created, so a key
                from an earlier visit cannot be shown again.
              </p>
              <Link to="/keys" className="firstrun-create-key-btn">
                Create an API key to continue
              </Link>
            </div>
          )}

          {/* Visually hidden live region for key-copy announcement. */}
          <div
            aria-live="assertive"
            aria-atomic="true"
            className="firstrun-sr-live"
          >
            {keyAnnounce}
          </div>
        </section>

        {/* Step 2: pick a runtime and copy the snippet */}
        <section
          data-step="snippet"
          data-done={snippetDone ? 'true' : undefined}
          className="firstrun-step"
          aria-label="Step 2: run a snippet"
        >
          <div className="firstrun-step-header">
            <span className="firstrun-step-num" aria-hidden="true">2</span>
            <p className="firstrun-step-title">Run it</p>
            {snippetDone && (
              <span className="firstrun-step-check" aria-hidden="true">
                {'✓'}
              </span>
            )}
          </div>

          <div className="firstrun-tab-bar">
            <Tabs
              tabs={RUNTIMES.map((r) => ({ key: r.id, label: r.label }))}
              active={runtime}
              onChange={(k) => setRuntime(k as Runtime)}
              ariaLabel="Runtime"
            />
          </div>

          <div className="firstrun-snippet-block" role="region" aria-label="Runnable snippet">
            <code className="firstrun-snippet-code">{content.snippets[runtime]}</code>
            <button
              type="button"
              className="firstrun-copy-btn"
              onClick={() => void handleSnippetCopy()}
              aria-label={
                snippetCopied ? 'Copied to clipboard' : 'Copy snippet to clipboard'
              }
            >
              {snippetCopyFailed
                ? 'Copy failed, select the text'
                : snippetCopied
                  ? 'Copied'
                  : 'Copy snippet'}
            </button>
            {snippetCopyFailed && (
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

          {/* Visually hidden live region for snippet-copy announcement. */}
          <div
            aria-live="assertive"
            aria-atomic="true"
            className="firstrun-sr-live"
          >
            {snippetAnnounce}
          </div>
        </section>

        {/* Step 3: wait for the first call, then celebrate */}
        <section
          data-step="activity"
          data-done={active ? 'true' : undefined}
          className="firstrun-step"
          aria-label="Step 3: make your first call"
        >
          <div className="firstrun-step-header">
            <span className="firstrun-step-num" aria-hidden="true">3</span>
            <p className="firstrun-step-title">First call</p>
            {active && (
              <span className="firstrun-step-check" aria-hidden="true">
                {'✓'}
              </span>
            )}
          </div>

          {!active && (
            <p className="firstrun-waiting">Waiting for your first call...</p>
          )}

          <Celebrate active={active} />

          {active && (
            <ul className="firstrun-next-links">
              <li>
                <Link to="/forks" className="firstrun-next-link">
                  Open the fork tree
                </Link>
              </li>
              <li>
                <Link to="/usage" className="firstrun-next-link">
                  View usage
                </Link>
              </li>
              <li>
                <Link to="/billing" className="firstrun-next-link">
                  Add credits
                </Link>
              </li>
            </ul>
          )}
        </section>
      </Card>
    </>
  )
}
