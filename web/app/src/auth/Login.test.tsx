import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, screen, fireEvent, waitFor } from '@testing-library/react'
import { Login } from './Login'

// mockConnectors stubs fetch so that GET /auth/connectors returns the given
// connector list. Other fetch calls fall through to the test's own stub or
// return a generic rejection so they do not accidentally succeed.
function mockConnectors(connectors: string[]) {
  vi.stubGlobal(
    'fetch',
    vi.fn().mockImplementation((url: string) => {
      if (url === '/auth/connectors') {
        return Promise.resolve({
          ok: true,
          status: 200,
          json: async () => ({ connectors }),
        })
      }
      return Promise.reject(new Error(`unexpected fetch: ${url}`))
    }),
  )
}

describe('Login page', () => {
  beforeEach(() => {
    vi.restoreAllMocks()
  })

  it('renders a GitHub button and no Google button when only github is configured', async () => {
    mockConnectors(['github'])
    render(<Login />)
    expect(
      await screen.findByRole('link', { name: /Continue with GitHub/i }),
    ).toHaveAttribute('href', '/auth/login?connector=github')
    expect(screen.queryByRole('link', { name: /Continue with Google/i })).not.toBeInTheDocument()
  })

  it('renders both buttons when github and google are configured', async () => {
    mockConnectors(['github', 'google'])
    render(<Login />)
    expect(
      await screen.findByRole('link', { name: /Continue with GitHub/i }),
    ).toHaveAttribute('href', '/auth/login?connector=github')
    expect(
      await screen.findByRole('link', { name: /Continue with Google/i }),
    ).toHaveAttribute('href', '/auth/login?connector=google')
  })

  it('renders no social buttons when connectors list is empty', async () => {
    mockConnectors([])
    render(<Login />)
    // Wait for the fetch to settle, then verify no provider links appear.
    await waitFor(() => {
      expect(screen.queryByRole('link', { name: /Continue with GitHub/i })).not.toBeInTheDocument()
      expect(screen.queryByRole('link', { name: /Continue with Google/i })).not.toBeInTheDocument()
    })
  })

  it('renders an email input', async () => {
    mockConnectors([])
    render(<Login />)
    // Use waitFor to flush the connectors fetch microtask before asserting.
    await waitFor(() => {
      expect(screen.getByRole('textbox', { name: /email/i })).toBeInTheDocument()
    })
  })

  it('renders a signup affordance', async () => {
    mockConnectors([])
    render(<Login />)
    await waitFor(() => {
      expect(screen.getByRole('button', { name: /sign up with email/i })).toBeInTheDocument()
    })
  })

  it('propagates ?next= to provider connector hrefs', async () => {
    mockConnectors(['github', 'google'])
    render(<Login next="/dashboard" />)
    expect(
      await screen.findByRole('link', { name: /Continue with GitHub/i }),
    ).toHaveAttribute('href', '/auth/login?connector=github&next=%2Fdashboard')
    expect(
      await screen.findByRole('link', { name: /Continue with Google/i }),
    ).toHaveAttribute('href', '/auth/login?connector=google&next=%2Fdashboard')
  })

  it('propagates next= on email form submission', async () => {
    mockConnectors([])
    const assign = vi.fn()
    vi.stubGlobal('location', { ...window.location, assign })
    render(<Login next="/dashboard" />)
    // Wait for connectors fetch to settle before interacting.
    await waitFor(() => {
      expect(screen.getByRole('form', { name: /continue with email/i })).toBeInTheDocument()
    })
    fireEvent.submit(screen.getByRole('form', { name: /continue with email/i }))
    expect(assign).toHaveBeenCalledWith(expect.stringContaining('next=%2Fdashboard'))
    vi.unstubAllGlobals()
  })
})
