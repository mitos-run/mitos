// Signup page tests. Mirrors Login.test.tsx conventions.
import { describe, it, expect, vi } from 'vitest'
import { render, screen, fireEvent, waitFor } from '@testing-library/react'
import { Signup } from './Signup'

describe('Signup page', () => {
  it('offers GitHub and Google provider options', () => {
    render(<Signup />)
    expect(screen.getByRole('link', { name: /Continue with GitHub/i })).toBeInTheDocument()
    expect(screen.getByRole('link', { name: /Continue with Google/i })).toBeInTheDocument()
  })

  it('renders an email input with a visible label', () => {
    render(<Signup />)
    expect(screen.getByRole('textbox', { name: /email/i })).toBeInTheDocument()
  })

  it('renders the submit button', () => {
    render(<Signup />)
    expect(screen.getByRole('button', { name: /send me a sign-in link/i })).toBeInTheDocument()
  })

  it('prefills email from initialEmail prop (simulates ?email= query param)', () => {
    render(<Signup initialEmail="prefilled@example.com" />)
    expect(screen.getByRole('textbox', { name: /email/i })).toHaveValue('prefilled@example.com')
  })

  it('shows confirmation after successful submit (202) and calls fetch with the email', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn().mockResolvedValue({ ok: true, status: 202, text: async () => '' }),
    )
    render(<Signup />)
    const input = screen.getByRole('textbox', { name: /email/i })
    fireEvent.change(input, { target: { value: 'user@example.com' } })
    fireEvent.submit(input.closest('form')!)
    await waitFor(() =>
      expect(screen.getByText(/check your email/i)).toBeInTheDocument(),
    )
    expect(fetch).toHaveBeenCalledWith(
      '/onboarding/signup',
      expect.objectContaining({
        method: 'POST',
        body: JSON.stringify({ email: 'user@example.com' }),
      }),
    )
    vi.unstubAllGlobals()
  })

  it('echoes the submitted email in the confirmation message', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn().mockResolvedValue({ ok: true, status: 202, text: async () => '' }),
    )
    render(<Signup />)
    const input = screen.getByRole('textbox', { name: /email/i })
    fireEvent.change(input, { target: { value: 'jannes@example.com' } })
    fireEvent.submit(input.closest('form')!)
    await waitFor(() =>
      expect(screen.getByText(/jannes@example\.com/)).toBeInTheDocument(),
    )
    vi.unstubAllGlobals()
  })

  it('shows a non-leaky error message on failure and does not expose whether the email exists', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn().mockResolvedValue({ ok: false, status: 400, text: async () => '' }),
    )
    render(<Signup />)
    const input = screen.getByRole('textbox', { name: /email/i })
    fireEvent.change(input, { target: { value: 'bad@example.com' } })
    fireEvent.submit(input.closest('form')!)
    await waitFor(() =>
      expect(screen.getByText(/something went wrong/i)).toBeInTheDocument(),
    )
    // No enumeration leakage: must not reveal whether the address is registered
    expect(screen.queryByText(/already registered/i)).not.toBeInTheDocument()
    expect(screen.queryByText(/not found/i)).not.toBeInTheDocument()
    expect(screen.queryByText(/does not exist/i)).not.toBeInTheDocument()
    vi.unstubAllGlobals()
  })

  it('propagates ?next= to provider connector hrefs', () => {
    render(<Signup next="/dashboard" />)
    expect(screen.getByRole('link', { name: /Continue with GitHub/i })).toHaveAttribute(
      'href',
      '/auth/login?connector=github&next=%2Fdashboard',
    )
    expect(screen.getByRole('link', { name: /Continue with Google/i })).toHaveAttribute(
      'href',
      '/auth/login?connector=google&next=%2Fdashboard',
    )
  })

  it('renders a link to the sign-in page', () => {
    render(<Signup />)
    expect(screen.getByRole('link', { name: /sign in/i })).toBeInTheDocument()
  })

  it('disables the submit button while the POST is in flight', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn().mockReturnValue(new Promise(() => {})), // never resolves
    )
    render(<Signup />)
    const input = screen.getByRole('textbox', { name: /email/i })
    fireEvent.change(input, { target: { value: 'test@example.com' } })
    fireEvent.submit(input.closest('form')!)
    await waitFor(() =>
      expect(screen.getByRole('button', { name: /sending/i })).toBeDisabled(),
    )
    vi.unstubAllGlobals()
  })

  it('clicking "Use a different email" resets back to the form', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn().mockResolvedValue({ ok: true, status: 202, text: async () => '' }),
    )
    render(<Signup />)
    const input = screen.getByRole('textbox', { name: /email/i })
    fireEvent.change(input, { target: { value: 'user@example.com' } })
    fireEvent.submit(input.closest('form')!)
    await waitFor(() =>
      expect(screen.getByText(/check your email/i)).toBeInTheDocument(),
    )
    fireEvent.click(screen.getByRole('button', { name: /use a different email/i }))
    expect(screen.getByRole('textbox', { name: /email/i })).toBeInTheDocument()
    expect(screen.queryByText(/check your email/i)).not.toBeInTheDocument()
    vi.unstubAllGlobals()
  })
})
