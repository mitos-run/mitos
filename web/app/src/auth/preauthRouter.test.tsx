import { describe, it, expect, vi } from 'vitest'
import { render, screen } from '@testing-library/react'
import { RouterProvider } from '@tanstack/react-router'
import { createPreAuthRouter } from './preauthRouter'

// Stub fetch so GET /auth/connectors returns the given connector list (and,
// when provided, the server-controlled signup flag) for the router tests.
// This makes the Login/Signup pages show the GitHub button, which the tests
// verify below. Unstubbing is handled by the restoreMocks: true vitest config.
function stubConnectors(connectors: string[], signup?: boolean) {
  vi.stubGlobal(
    'fetch',
    vi.fn().mockImplementation((url: string) => {
      if (url === '/auth/connectors') {
        return Promise.resolve({
          ok: true,
          status: 200,
          json: async () => ({ connectors, ...(signup === undefined ? {} : { signup }) }),
        })
      }
      return Promise.reject(new Error(`unexpected fetch: ${url}`))
    }),
  )
}

describe('pre-auth router', () => {
  it('renders the login route with a GitHub affordance when github is configured', async () => {
    stubConnectors(['github'])
    const router = createPreAuthRouter('/login')
    render(<RouterProvider router={router} />)
    expect(await screen.findByText(/Continue with GitHub/i)).toBeInTheDocument()
  })

  it('does not render a Google button when only github is configured', async () => {
    stubConnectors(['github'])
    const router = createPreAuthRouter('/login')
    render(<RouterProvider router={router} />)
    await screen.findByText(/Continue with GitHub/i) // wait for connectors to load
    expect(screen.queryByText(/Continue with Google/i)).not.toBeInTheDocument()
  })

  it('redirects unmatched paths to the login affordance', async () => {
    stubConnectors(['github'])
    const router = createPreAuthRouter('/sandboxes')
    render(<RouterProvider router={router} />)
    expect(await screen.findByText(/Continue with GitHub/i)).toBeInTheDocument()
  })

  it('/signup stays reachable but shows the administrator message when signup is disabled', async () => {
    stubConnectors(['github'], false)
    const router = createPreAuthRouter('/signup')
    render(<RouterProvider router={router} />)
    expect(await screen.findByText(/handled by your administrator/i)).toBeInTheDocument()
    expect(screen.queryByRole('textbox', { name: /email/i })).not.toBeInTheDocument()
  })

  it('/signup shows the signup form when signup is enabled', async () => {
    stubConnectors(['github'], true)
    const router = createPreAuthRouter('/signup')
    render(<RouterProvider router={router} />)
    expect(await screen.findByRole('textbox', { name: /email/i })).toBeInTheDocument()
  })
})
