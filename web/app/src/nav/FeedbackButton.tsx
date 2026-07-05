// One-click feedback: a quiet speech-bubble icon button in TopBar that opens
// a small dialog with a message textarea and a transparent, read-only preview
// of the diagnostics block that gets attached (the user sees exactly what is
// sent, nothing hidden). There is NO server write path in v1: nothing here is
// persisted or audited. Sending hands the composed message straight to the
// OS mail client (channel "email", the hosted default, caps.feedback from
// GET /console/capabilities) or opens a prefilled GitHub new-issue tab
// (channel "github", the community default).
//
// A11y: role="dialog" + aria-modal, labelled by the heading, Escape closes,
// focus moves into the dialog (the textarea) on open, Tab/Shift+Tab is
// trapped within it, and focus returns to the feedback button on close, all
// via the shared useModalFocus hook, matching InviteModal's pattern (a plain
// confirm dialog elsewhere in the app was flagged for skipping this; every
// new dialog does it right from the start).
import { useEffect, useRef, useState } from 'react'
import { Button } from '@mitos/brand'
import type { Capabilities } from '../api'
import { collectDiagnostics } from '../lib/diagnostics'
import { useModalFocus } from '../ui/useModalFocus'

function SpeechBubbleIcon() {
  return (
    <svg width="18" height="18" viewBox="0 0 20 20" fill="none" aria-hidden="true" focusable="false">
      <path
        d="M3 4.5A1.5 1.5 0 0 1 4.5 3h11A1.5 1.5 0 0 1 17 4.5v7A1.5 1.5 0 0 1 15.5 13H8.2L4.5 16.5V13H4.5A1.5 1.5 0 0 1 3 11.5v-7Z"
        stroke="currentColor"
        strokeWidth="1.4"
        strokeLinejoin="round"
        strokeLinecap="round"
      />
    </svg>
  )
}

export function FeedbackButton({ caps, route }: { caps: Capabilities; route: string }) {
  const [open, setOpen] = useState(false)
  const triggerRef = useRef<HTMLButtonElement>(null)

  // Hide entirely on an older server that has not yet been upgraded to
  // advertise a feedback channel, rather than rendering a button that goes
  // nowhere (no dead ends).
  if (!caps.feedback) return null

  return (
    <>
      <button
        ref={triggerRef}
        type="button"
        className="feedback-button"
        aria-label="Send feedback"
        onClick={() => setOpen(true)}
      >
        <SpeechBubbleIcon />
      </button>
      {open && (
        <FeedbackDialog
          caps={caps}
          feedback={caps.feedback}
          route={route}
          triggerRef={triggerRef}
          onClose={() => setOpen(false)}
        />
      )}
    </>
  )
}

function FeedbackDialog({
  caps,
  feedback,
  route,
  triggerRef,
  onClose,
}: {
  caps: Capabilities
  feedback: NonNullable<Capabilities['feedback']>
  route: string
  triggerRef: React.RefObject<HTMLButtonElement | null>
  onClose: () => void
}) {
  const [message, setMessage] = useState('')
  const textareaRef = useRef<HTMLTextAreaElement>(null)
  const dialogRef = useRef<HTMLDivElement>(null)
  const diagnostics = collectDiagnostics(caps, route)

  useModalFocus(dialogRef, { active: true, initialFocusRef: textareaRef, returnFocusRef: triggerRef })

  useEffect(() => {
    function onKeyDown(e: KeyboardEvent) {
      if (e.key === 'Escape') onClose()
    }
    document.addEventListener('keydown', onKeyDown)
    return () => document.removeEventListener('keydown', onKeyDown)
  }, [onClose])

  function send() {
    const trimmed = message.trim()
    const body = trimmed ? `${trimmed}\n\n${diagnostics}` : diagnostics
    if (feedback.channel === 'email') {
      // feedback.target is server-configured (GET /console/capabilities), not
      // user input, but it is still interpolated into a URL: encode it so a
      // stray "?"/"&" in a misconfigured target can never break out of the
      // mailto to-part and corrupt the subject/body that follow.
      const url = `mailto:${encodeURIComponent(feedback.target)}?subject=${encodeURIComponent('Mitos feedback')}&body=${encodeURIComponent(body)}`
      window.location.assign(url)
    } else {
      const title = trimmed.slice(0, 60) || 'Feedback'
      // target is "owner/repo": encode each path segment individually so the
      // one meaningful "/" is preserved while any other reserved character
      // within a segment is neutralized (same reasoning as the mailto case).
      const targetPath = feedback.target.split('/').map(encodeURIComponent).join('/')
      const url = `https://github.com/${targetPath}/issues/new?title=${encodeURIComponent(title)}&body=${encodeURIComponent(body)}`
      window.open(url, '_blank')
    }
    onClose()
  }

  return (
    <div
      className="modal-backdrop"
      onMouseDown={(e) => {
        if (e.target === e.currentTarget) onClose()
      }}
    >
      <div ref={dialogRef} role="dialog" aria-modal="true" aria-labelledby="feedback-dialog-title" className="card modal">
        <h2 id="feedback-dialog-title" style={{ marginTop: 0, marginBottom: 'var(--space-2)' }}>
          Send feedback
        </h2>
        <p className="t-dim" style={{ marginTop: 0, marginBottom: 'var(--space-5)' }}>
          {feedback.channel === 'email'
            ? `Opens an email to ${feedback.target}.`
            : `Opens a new GitHub issue in ${feedback.target}.`}
        </p>

        <div className="form-row" style={{ marginBottom: 'var(--space-4)' }}>
          <label htmlFor="feedback-message">What happened, or what would make Mitos better?</label>
          <textarea
            id="feedback-message"
            ref={textareaRef}
            rows={5}
            value={message}
            onChange={(e) => setMessage(e.target.value)}
            style={{ width: '100%', resize: 'vertical', font: 'inherit' }}
          />
        </div>

        <details style={{ marginBottom: 'var(--space-5)' }}>
          <summary className="t-dim" style={{ cursor: 'pointer' }}>
            Diagnostics attached
          </summary>
          <pre
            style={{
              whiteSpace: 'pre-wrap',
              wordBreak: 'break-word',
              fontFamily: 'var(--mono)',
              fontSize: 'var(--step--1)',
              color: 'var(--ink-2)',
              marginTop: 'var(--space-2)',
            }}
          >
            {diagnostics}
          </pre>
        </details>

        <div style={{ display: 'flex', gap: 'var(--space-3)', justifyContent: 'flex-end' }}>
          <button type="button" className="btn btn-ghost" onClick={onClose}>
            Cancel
          </button>
          <Button type="button" variant="primary" onClick={send}>
            Send feedback
          </Button>
        </div>
      </div>
    </div>
  )
}
