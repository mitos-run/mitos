// Signup page tests. Mirrors Login.test.tsx conventions.
import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, screen, fireEvent, waitFor } from '@testing-library/react'
import { Signup } from './Signup'

// mockFetch stubs global fetch so /auth/connectors returns the given list and
// POST /onboarding/signup returns the given response. Extra URLs return a
// rejection so unintended calls surface immediately.
function mockFetch({
  connectors = [] as string[],
  signupStatus = 202,
} = {}) {
  vi.stubGlobal(
    'fetch',
    vi.fn().mockImplementation((url: string, opts?: RequestInit) => {
      if (url === '/auth/connectors') {
        return Promise.resolve({
          ok: true,
          status: 200,
          json: async () => ({ connectors }),
        })
      }
      if (url === '/onboarding/signup' && opts?.method === 'POST') {
        return Promise.resolve({
          ok: signupStatus < 400,
          status: signupStatus,
          text: async () => '',
        })
      }
      return Promise.reject(new Error(`unexpected fetch: ${url}`))
    }),
  )
}

describe('Signup page', () => {
  beforeEach(() => {
    vi.restoreAllMocks()
  })

  it('offers GitHub button when only github is configured', async () => {
    mockFetch({ connectors: ['github'] })
    render(<Signup />)
    expect(
      await screen.findByRole('link', { name: /Continue with GitHub/i }),
    ).toBeInTheDocument()
    expect(screen.queryByRole('link', { name: /Continue with Google/i })).not.toBeInTheDocument()
  })

  it('offers GitHub and Google when both are configured', async () => {
    mockFetch({ connectors: ['github', 'google'] })
    render(<Signup />)
    expect(await screen.findByRole('link', { name: /Continue with GitHub/i })).toBeInTheDocument()
    expect(await screen.findByRole('link', { name: /Continue with Google/i })).toBeInTheDocument()
  })

  it('renders no social buttons when connectors list is empty', async () => {
    mockFetch({ connectors: [] })
    render(<Signup />)
    await waitFor(() => {
      expect(screen.queryByRole('link', { name: /Continue with GitHub/i })).not.toBeInTheDocument()
      expect(screen.queryByRole('link', { name: /Continue with Google/i })).not.toBeInTheDocument()
    })
  })

  it('renders an email input with a visible label', async () => {
    mockFetch()
    render(<Signup />)
    await waitFor(() => {
      expect(screen.getByRole('textbox', { name: /email/i })).toBeInTheDocument()
    })
  })

  it('renders the submit button', async () => {
    mockFetch()
    render(<Signup />)
    await waitFor(() => {
      expect(screen.getByRole('button', { name: /send me a sign-in link/i })).toBeInTheDocument()
    })
  })

  it('prefills email from initialEmail prop (simulates ?email= query param)', async () => {
    mockFetch()
    render(<Signup initialEmail="prefilled@example.com" />)
    await waitFor(() => {
      expect(screen.getByRole('textbox', { name: /email/i })).toHaveValue('prefilled@example.com')
    })
  })

  it('shows confirmation after successful submit (202) and calls fetch with the email', async () => {
    mockFetch({ signupStatus: 202 })
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
    mockFetch({ signupStatus: 202 })
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
    mockFetch({ signupStatus: 400 })
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

  it('propagates ?next= to provider connector hrefs', async () => {
    mockFetch({ connectors: ['github', 'google'] })
    render(<Signup next="/dashboard" />)
    expect(
      await screen.findByRole('link', { name: /Continue with GitHub/i }),
    ).toHaveAttribute('href', '/auth/login?connector=github&next=%2Fdashboard')
    expect(
      await screen.findByRole('link', { name: /Continue with Google/i }),
    ).toHaveAttribute('href', '/auth/login?connector=google&next=%2Fdashboard')
  })

  it('renders a link to the sign-in page', async () => {
    mockFetch()
    render(<Signup />)
    await waitFor(() => {
      expect(screen.getByRole('link', { name: /sign in/i })).toBeInTheDocument()
    })
  })

  it('disables the submit button while the POST is in flight', async () => {
    // connectors resolves immediately; signup never resolves (in-flight)
    vi.stubGlobal(
      'fetch',
      vi.fn().mockImplementation((url: string) => {
        if (url === '/auth/connectors') {
          return Promise.resolve({
            ok: true,
            status: 200,
            json: async () => ({ connectors: [] }),
          })
        }
        return new Promise(() => {}) // never resolves
      }),
    )
    render(<Signup />)
    // Wait for connectors fetch to settle before interacting.
    await waitFor(() => {
      expect(screen.getByRole('textbox', { name: /email/i })).toBeInTheDocument()
    })
    const input = screen.getByRole('textbox', { name: /email/i })
    fireEvent.change(input, { target: { value: 'test@example.com' } })
    fireEvent.submit(input.closest('form')!)
    await waitFor(() =>
      expect(screen.getByRole('button', { name: /sending/i })).toBeDisabled(),
    )
    vi.unstubAllGlobals()
  })

  it('includes uc in POST body when ?uc=ai-coding is in the URL', async () => {
    Object.defineProperty(window, 'location', {
      value: { ...window.location, search: '?uc=ai-coding' },
      writable: true,
      configurable: true,
    })
    vi.stubGlobal(
      'fetch',
      vi.fn().mockResolvedValue({ ok: true, status: 202, text: async () => '' }),
    )
    render(<Signup />)
    const input = screen.getByRole('textbox', { name: /email/i })
    fireEvent.change(input, { target: { value: 'user@example.com' } })
    fireEvent.submit(input.closest('form')!)
    await waitFor(() =>
      expect(fetch).toHaveBeenCalledWith(
        '/onboarding/signup',
        expect.objectContaining({
          method: 'POST',
          body: JSON.stringify({ email: 'user@example.com', uc: 'ai-coding' }),
        }),
      ),
    )
    Object.defineProperty(window, 'location', {
      value: { ...window.location, search: '' },
      writable: true,
      configurable: true,
    })
    vi.unstubAllGlobals()
  })

  it('clicking "Use a different email" resets back to the form', async () => {
    mockFetch({ signupStatus: 202 })
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
