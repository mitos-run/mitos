// Behavior tests for InviteModal: multi-email parsing, per-address send
// results (so a partial failure never silently drops the rest), role
// selection, and Escape-to-close.
import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, fireEvent, waitFor, screen } from '@testing-library/react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { InviteModal } from './InviteModal'
import type { InvitationView } from '../../api'

function wrap(ui: React.ReactElement) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  return render(<QueryClientProvider client={client}>{ui}</QueryClientProvider>)
}

const created: InvitationView = {
  id: 'inv-1',
  org_id: 'o1',
  email: 'ada@example.com',
  role: 'member',
  state: 'pending',
  inviter_id: 'a1',
  inviter_name: 'Alice',
  created_at: '2026-01-01T00:00:00Z',
  expires_at: '2026-01-08T00:00:00Z',
}

beforeEach(() => {
  vi.spyOn(globalThis, 'fetch').mockImplementation((input, init) => {
    const url = String(input)
    const method = (init?.method ?? 'GET').toUpperCase()
    if (url.endsWith('/console/invites') && method === 'POST') {
      const body = JSON.parse(String(init?.body ?? '{}'))
      if (body.email === 'fail@example.com') {
        return Promise.resolve(
          new Response(JSON.stringify({ error: { cause: 'an invitation is already pending for this email' } }), {
            status: 400,
            headers: { 'content-type': 'application/json' },
          }),
        )
      }
      return Promise.resolve(
        new Response(JSON.stringify({ ...created, email: body.email, role: body.role }), {
          status: 201,
          headers: { 'content-type': 'application/json' },
        }),
      )
    }
    return Promise.resolve(new Response(JSON.stringify({}), { status: 200, headers: { 'content-type': 'application/json' } }))
  })
})

describe('InviteModal', () => {
  it('renders a labelled email textarea, role select, and send button, focusing the textarea', async () => {
    wrap(<InviteModal onClose={vi.fn()} />)
    const textarea = screen.getByLabelText(/email addresses/i)
    expect(textarea).toBeInTheDocument()
    expect(textarea).toHaveFocus()
    expect(screen.getByLabelText(/role/i)).toBeInTheDocument()
    expect(screen.getByRole('button', { name: /send invite/i })).toBeInTheDocument()
  })

  it('sends one invite per newline/comma-separated address and reports success per row', async () => {
    wrap(<InviteModal onClose={vi.fn()} />)
    fireEvent.change(screen.getByLabelText(/email addresses/i), {
      target: { value: 'ada@example.com, grace@example.com' },
    })
    fireEvent.click(screen.getByRole('button', { name: /send invites/i }))
    await waitFor(() => expect(screen.getByText(/sent to ada@example.com/i)).toBeInTheDocument())
    expect(screen.getByText(/sent to grace@example.com/i)).toBeInTheDocument()
  })

  it('reports a per-address failure without dropping the successful ones', async () => {
    wrap(<InviteModal onClose={vi.fn()} />)
    fireEvent.change(screen.getByLabelText(/email addresses/i), {
      target: { value: 'ada@example.com\nfail@example.com' },
    })
    fireEvent.click(screen.getByRole('button', { name: /send invites/i }))
    await waitFor(() => expect(screen.getByText(/sent to ada@example.com/i)).toBeInTheDocument())
    expect(screen.getByText(/failed for fail@example.com/i)).toBeInTheDocument()
  })

  it('closes on Escape', () => {
    const onClose = vi.fn()
    wrap(<InviteModal onClose={onClose} />)
    fireEvent.keyDown(document, { key: 'Escape' })
    expect(onClose).toHaveBeenCalled()
  })

  it('traps Tab from the last button back to the textarea, and Shift+Tab from the textarea to the last button', () => {
    wrap(<InviteModal onClose={vi.fn()} />)
    const textarea = screen.getByLabelText(/email addresses/i)
    // Enable the Send button (disabled with no address entered) so it is a
    // real focusable, and therefore the last element in tab order.
    fireEvent.change(textarea, { target: { value: 'ada@example.com' } })
    const sendButton = screen.getByRole('button', { name: /send invite/i })
    sendButton.focus()
    fireEvent.keyDown(document, { key: 'Tab' })
    expect(textarea).toHaveFocus()
    fireEvent.keyDown(document, { key: 'Tab', shiftKey: true })
    expect(sendButton).toHaveFocus()
  })

  it('returns focus to the trigger once the modal unmounts', () => {
    const trigger = document.createElement('button')
    document.body.appendChild(trigger)
    trigger.focus()
    const { unmount } = wrap(<InviteModal onClose={vi.fn()} />)
    unmount()
    expect(trigger).toHaveFocus()
    trigger.remove()
  })

  it('disables the send button until at least one email is entered', () => {
    wrap(<InviteModal onClose={vi.fn()} />)
    expect(screen.getByRole('button', { name: /send invite/i })).toBeDisabled()
    fireEvent.change(screen.getByLabelText(/email addresses/i), { target: { value: 'ada@example.com' } })
    expect(screen.getByRole('button', { name: /send invite/i })).not.toBeDisabled()
  })

  // Mobile: the dialog carries the shared .modal class so base.css's
  // <=480px media query turns it into a full-screen sheet (100dvw/100dvh,
  // safe-area padding) instead of a small floating card that can clip on a
  // phone. The backdrop carries .modal-backdrop so its own padding collapses
  // to 0 at that breakpoint (see base.css).
  it('carries the shared modal classes for the mobile full-screen sheet treatment', () => {
    wrap(<InviteModal onClose={vi.fn()} />)
    const dialog = screen.getByRole('dialog')
    expect(dialog.className).toContain('modal')
    expect(dialog.parentElement?.className).toContain('modal-backdrop')
  })
})
