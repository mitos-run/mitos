// useModalFocus: the one focus-management utility every dialog/panel in the
// app shares, so "trap Tab, focus the right field on open, give focus back
// to whatever opened it" is implemented once instead of copy-pasted (and
// drifting) across InviteModal, NewSandboxModal, FeedbackButton's dialog, the
// Members remove/leave confirm, and the ForkTree detail panel.
//
// Deliberately NOT an aria-hide-the-rest-of-the-page full trap library: the
// app's dialogs render a visual backdrop but never aria-hide the background,
// so this only needs to (1) move focus to a designated element on open, (2)
// keep Tab/Shift+Tab cycling within the container while active, and (3)
// return focus to the trigger once the dialog/panel closes.
import { useEffect, type RefObject } from 'react'

const FOCUSABLE_SELECTOR = [
  'a[href]',
  'button:not([disabled])',
  'textarea:not([disabled])',
  'input:not([disabled])',
  'select:not([disabled])',
  '[tabindex]:not([tabindex="-1"])',
].join(', ')

function focusableElements(container: HTMLElement): HTMLElement[] {
  // Deliberately not filtered by layout visibility (offsetParent/getClientRects):
  // jsdom never computes layout, so any such check would make every element
  // "invisible" under test. Explicit hiding (the hidden attribute or an
  // inline display:none) is enough for the cases this app renders.
  return Array.from(container.querySelectorAll<HTMLElement>(FOCUSABLE_SELECTOR)).filter(
    (el) => !el.hidden && el.style.display !== 'none',
  )
}

export type UseModalFocusOptions = {
  // Whether the dialog/panel is currently open. The hook does its
  // open-focus/trap/close-restore dance on the true->false and mount->unmount
  // transitions; pass a stable `true` for a component that is only ever
  // mounted while open (the common case here).
  active: boolean
  // Element to focus first. Falls back to the first focusable descendant of
  // the container when omitted or not yet mounted.
  initialFocusRef?: RefObject<HTMLElement | null>
  // Element to return focus to once the dialog/panel closes. Falls back to
  // whatever had focus at the moment this hook activated (typically the
  // trigger that opened it, if the trigger's own click handler focused
  // itself first).
  returnFocusRef?: RefObject<HTMLElement | null>
  // Whether Tab/Shift+Tab should cycle within the container. Defaults to
  // true; a non-modal panel (ForkTree's detail panel) that only wants the
  // return-focus behavior can pass false.
  trap?: boolean
}

export function useModalFocus(containerRef: RefObject<HTMLElement | null>, options: UseModalFocusOptions): void {
  const { active, initialFocusRef, returnFocusRef, trap = true } = options

  useEffect(() => {
    if (!active) return

    const returnTarget =
      returnFocusRef?.current ?? (document.activeElement instanceof HTMLElement ? document.activeElement : null)

    const container = containerRef.current
    const initial = initialFocusRef?.current ?? (container ? focusableElements(container)[0] : undefined)
    initial?.focus()

    function onKeyDown(e: KeyboardEvent) {
      if (!trap || e.key !== 'Tab') return
      const el = containerRef.current
      if (!el) return
      const focusables = focusableElements(el)
      if (focusables.length === 0) {
        e.preventDefault()
        return
      }
      const first = focusables[0]
      const last = focusables[focusables.length - 1]
      const current = document.activeElement
      if (e.shiftKey) {
        if (current === first || !(current instanceof Node) || !el.contains(current)) {
          e.preventDefault()
          last.focus()
        }
      } else if (current === last || !(current instanceof Node) || !el.contains(current)) {
        e.preventDefault()
        first.focus()
      }
    }
    document.addEventListener('keydown', onKeyDown)

    return () => {
      document.removeEventListener('keydown', onKeyDown)
      if (returnTarget && document.contains(returnTarget)) {
        returnTarget.focus()
      }
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps -- refs are stable
    // identities; re-running this effect on every render would re-capture
    // returnTarget and re-focus initial on each keystroke.
  }, [active, trap])
}
