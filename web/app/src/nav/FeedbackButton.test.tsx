// Behavior tests for FeedbackButton: channel-aware composition (mailto vs
// GitHub new-issue), the diagnostics preview (transparency: the user sees
// exactly what is attached), and dialog a11y (focus-on-open, Escape-to-close).
import { describe, it, expect, vi, afterEach } from 'vitest'
import { render, fireEvent, screen, within } from '@testing-library/react'
import { FeedbackButton } from './FeedbackButton'
import type { Capabilities } from '../api'

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
  version: '1.6.0',
}

const emailCaps: Capabilities = { ...baseCaps, feedback: { channel: 'email', target: 'feedback@mitos.run' } }
const githubCaps: Capabilities = {
  ...baseCaps,
  edition: 'community',
  ownership: 'self-hosted',
  feedback: { channel: 'github', target: 'mitos-run/mitos' },
}

afterEach(() => {
  vi.restoreAllMocks()
})

describe('FeedbackButton', () => {
  it('renders nothing when the server does not advertise a feedback channel (older server)', () => {
    const { container } = render(<FeedbackButton caps={baseCaps} route="/" />)
    expect(container.firstChild).toBeNull()
  })

  it('has an accessible label and opens the dialog on click, focusing the textarea', () => {
    render(<FeedbackButton caps={emailCaps} route="/billing" />)
    const button = screen.getByRole('button', { name: /send feedback/i })
    fireEvent.click(button)
    const textarea = screen.getByLabelText(/what happened, or what would make mitos better/i)
    expect(textarea).toBeInTheDocument()
    expect(textarea).toHaveFocus()
    expect(screen.getByRole('dialog')).toHaveAttribute('aria-modal', 'true')
  })

  it('closes on Escape', () => {
    render(<FeedbackButton caps={emailCaps} route="/billing" />)
    fireEvent.click(screen.getByRole('button', { name: /send feedback/i }))
    expect(screen.getByRole('dialog')).toBeInTheDocument()
    fireEvent.keyDown(document, { key: 'Escape' })
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
  })

  it('shows the exact diagnostics text in a read-only details block', () => {
    render(<FeedbackButton caps={emailCaps} route="/billing" />)
    fireEvent.click(screen.getByRole('button', { name: /send feedback/i }))
    expect(screen.getByText(/diagnostics attached/i)).toBeInTheDocument()
    // The version and route must appear verbatim in the preview: this is the
    // exact block that gets attached, not a summary.
    expect(screen.getByText(/1\.6\.0/)).toBeInTheDocument()
    expect(screen.getByText(/\/billing/)).toBeInTheDocument()
  })

  it('follows caps: email channel shows a mailto hint and composes a mailto: URL on send', () => {
    const assign = vi.fn()
    vi.stubGlobal('location', { ...window.location, assign })
    render(<FeedbackButton caps={emailCaps} route="/billing" />)
    fireEvent.click(screen.getByRole('button', { name: /send feedback/i }))
    expect(screen.getByText(/feedback@mitos\.run/)).toBeInTheDocument()
    fireEvent.change(screen.getByLabelText(/what happened/i), { target: { value: 'The fork tree is slow' } })
    fireEvent.click(within(screen.getByRole('dialog')).getByRole('button', { name: /^send feedback$/i }))
    expect(assign).toHaveBeenCalledTimes(1)
    const url = assign.mock.calls[0][0] as string
    expect(url).toMatch(/^mailto:feedback@mitos\.run\?/)
    expect(url).toContain('subject=Mitos%20feedback')
    expect(url).toContain(encodeURIComponent('The fork tree is slow'))
    expect(url).toContain(encodeURIComponent('1.6.0'))
    vi.unstubAllGlobals()
  })

  it('follows caps: github channel shows a GitHub hint and opens a new-issue URL on send', () => {
    const openSpy = vi.spyOn(window, 'open').mockImplementation(() => null)
    render(<FeedbackButton caps={githubCaps} route="/sandboxes" />)
    fireEvent.click(screen.getByRole('button', { name: /send feedback/i }))
    expect(screen.getByText(/mitos-run\/mitos/)).toBeInTheDocument()
    fireEvent.change(screen.getByLabelText(/what happened/i), { target: { value: 'Fork warm pool exhausted under load' } })
    fireEvent.click(within(screen.getByRole('dialog')).getByRole('button', { name: /^send feedback$/i }))
    expect(openSpy).toHaveBeenCalledTimes(1)
    const [url, target] = openSpy.mock.calls[0]
    expect(target).toBe('_blank')
    expect(url).toMatch(/^https:\/\/github\.com\/mitos-run\/mitos\/issues\/new\?/)
    expect(url).toContain(`title=${encodeURIComponent('Fork warm pool exhausted under load')}`)
    expect(url).toContain(encodeURIComponent('/sandboxes'))
  })

  it('truncates a long message to 60 chars for the GitHub issue title', () => {
    const openSpy = vi.spyOn(window, 'open').mockImplementation(() => null)
    render(<FeedbackButton caps={githubCaps} route="/" />)
    fireEvent.click(screen.getByRole('button', { name: /send feedback/i }))
    const longMessage = 'x'.repeat(120)
    fireEvent.change(screen.getByLabelText(/what happened/i), { target: { value: longMessage } })
    fireEvent.click(within(screen.getByRole('dialog')).getByRole('button', { name: /^send feedback$/i }))
    const [url] = openSpy.mock.calls[0]
    const parsed = new URL(url as string)
    expect(parsed.searchParams.get('title')).toBe('x'.repeat(60))
  })
})
