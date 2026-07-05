// Behavior tests for useModalFocus: the shared dialog/panel focus utility
// every modal (InviteModal, NewSandboxModal, FeedbackButton, the Members
// remove confirm) and the ForkTree detail panel build on. Covers: initial
// focus on activation, Tab/Shift+Tab trapping at both edges, and returning
// focus to the trigger on deactivation.
import { describe, it, expect } from 'vitest'
import { useRef, useState } from 'react'
import { render, fireEvent, screen } from '@testing-library/react'
import { useModalFocus } from './useModalFocus'

function Harness({ trap = true, withInitialFocus = true }: { trap?: boolean; withInitialFocus?: boolean }) {
  const [open, setOpen] = useState(false)
  const containerRef = useRef<HTMLDivElement>(null)
  const initialRef = useRef<HTMLButtonElement>(null)
  const triggerRef = useRef<HTMLButtonElement>(null)

  return (
    <div>
      <button ref={triggerRef} type="button" onClick={() => setOpen(true)}>
        Open
      </button>
      {open && (
        <Dialog
          containerRef={containerRef}
          initialRef={withInitialFocus ? initialRef : undefined}
          triggerRef={triggerRef}
          trap={trap}
          onClose={() => setOpen(false)}
        />
      )}
    </div>
  )
}

function Dialog({
  containerRef,
  initialRef,
  triggerRef,
  trap,
  onClose,
}: {
  containerRef: React.RefObject<HTMLDivElement>
  initialRef?: React.RefObject<HTMLButtonElement>
  triggerRef: React.RefObject<HTMLButtonElement>
  trap: boolean
  onClose: () => void
}) {
  useModalFocus(containerRef, { active: true, initialFocusRef: initialRef, returnFocusRef: triggerRef, trap })
  return (
    <div ref={containerRef} role="dialog">
      <button ref={initialRef} type="button">
        First
      </button>
      <button type="button">Middle</button>
      <button type="button" onClick={onClose}>
        Last
      </button>
    </div>
  )
}

describe('useModalFocus', () => {
  it('moves focus to the designated initial element on open', () => {
    render(<Harness />)
    fireEvent.click(screen.getByRole('button', { name: 'Open' }))
    expect(screen.getByRole('button', { name: 'First' })).toHaveFocus()
  })

  it('falls back to the first focusable element when no initial ref is given', () => {
    render(<Harness withInitialFocus={false} />)
    fireEvent.click(screen.getByRole('button', { name: 'Open' }))
    expect(screen.getByRole('button', { name: 'First' })).toHaveFocus()
  })

  it('traps Tab from the last focusable element back to the first', () => {
    render(<Harness />)
    fireEvent.click(screen.getByRole('button', { name: 'Open' }))
    screen.getByRole('button', { name: 'Last' }).focus()
    fireEvent.keyDown(document, { key: 'Tab' })
    expect(screen.getByRole('button', { name: 'First' })).toHaveFocus()
  })

  it('traps Shift+Tab from the first focusable element to the last', () => {
    render(<Harness />)
    fireEvent.click(screen.getByRole('button', { name: 'Open' }))
    expect(screen.getByRole('button', { name: 'First' })).toHaveFocus()
    fireEvent.keyDown(document, { key: 'Tab', shiftKey: true })
    expect(screen.getByRole('button', { name: 'Last' })).toHaveFocus()
  })

  it('does not trap Tab when trap is disabled', () => {
    render(<Harness trap={false} />)
    fireEvent.click(screen.getByRole('button', { name: 'Open' }))
    screen.getByRole('button', { name: 'Last' }).focus()
    fireEvent.keyDown(document, { key: 'Tab' })
    // No trapping: jsdom does not itself move focus on a raw keydown, so
    // focus simply stays put rather than wrapping to First.
    expect(screen.getByRole('button', { name: 'First' })).not.toHaveFocus()
  })

  it('returns focus to the trigger that opened it once the dialog unmounts', () => {
    render(<Harness />)
    const trigger = screen.getByRole('button', { name: 'Open' })
    fireEvent.click(trigger)
    fireEvent.click(screen.getByRole('button', { name: 'Last' }))
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
    expect(trigger).toHaveFocus()
  })
})
