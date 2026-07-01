// Verify page tests. Mirrors Signup.test.tsx conventions.
// Mocks fetch (via vi.stubGlobal) so the post() helper in api.ts sees
// a controlled response without spinning up a real server.
import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, screen, waitFor } from '@testing-library/react'
import { Verify } from './Verify'

beforeEach(() => {
  sessionStorage.clear()
})

describe('Verify page', () => {
  it('(a) shows the API key and a Continue to console link on first-time success', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn().mockResolvedValue({
        ok: true,
        status: 200,
        text: async () =>
          JSON.stringify({
            accountId: 'a',
            orgId: 'o',
            email: 'e@x.com',
            alreadyDone: false,
            apiKey: 'mitos_live_abc123',
            apiKeyId: 'k1',
          }),
      }),
    )
    render(<Verify token="t" />)
    await waitFor(() =>
      expect(screen.getByText('mitos_live_abc123')).toBeInTheDocument(),
    )
    const continueLink = screen.getByRole('link', { name: /continue to console/i })
    expect(continueLink).toHaveAttribute('href', '/')
    vi.unstubAllGlobals()
  })

  it('(b) already-done state shows continue affordance without an API key', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn().mockResolvedValue({
        ok: true,
        status: 200,
        text: async () =>
          JSON.stringify({
            accountId: 'a',
            orgId: 'o',
            email: 'e@x.com',
            alreadyDone: true,
          }),
      }),
    )
    render(<Verify token="t" />)
    await waitFor(() =>
      expect(
        screen.getByRole('link', { name: /continue to console/i }),
      ).toHaveAttribute('href', '/'),
    )
    expect(screen.queryByText(/mitos_live/)).not.toBeInTheDocument()
    vi.unstubAllGlobals()
  })

  it('(d) shows invalid-link message and /signup link when no token is provided', () => {
    render(<Verify />)
    expect(screen.getByText(/invalid or has expired/i)).toBeInTheDocument()
    const signupLink = screen.getByRole('link', { name: /start over/i })
    expect(signupLink).toHaveAttribute('href', '/signup')
  })

  it('(e) continue link includes ?uc= when the response carries a useCase', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn().mockResolvedValue({
        ok: true,
        status: 200,
        text: async () =>
          JSON.stringify({
            accountId: 'a',
            orgId: 'o',
            email: 'e@x.com',
            alreadyDone: false,
            apiKey: 'mitos_live_abc123',
            apiKeyId: 'k1',
            useCase: 'rollouts',
          }),
      }),
    )
    render(<Verify token="t" />)
    await waitFor(() =>
      expect(screen.getByRole('link', { name: /continue to console/i })).toHaveAttribute(
        'href',
        '/?uc=rollouts',
      ),
    )
    vi.unstubAllGlobals()
  })

  it('(f) already-done + useCase: continue link carries ?uc', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn().mockResolvedValue({
        ok: true,
        status: 200,
        text: async () =>
          JSON.stringify({
            accountId: 'a',
            orgId: 'o',
            email: 'e@x.com',
            alreadyDone: true,
            useCase: 'rollouts',
          }),
      }),
    )
    render(<Verify token="t" />)
    await waitFor(() =>
      expect(
        screen.getByRole('link', { name: /continue to console/i }),
      ).toHaveAttribute('href', '/?uc=rollouts'),
    )
    vi.unstubAllGlobals()
  })

  it('(g) successful verify with first key stashes it in sessionStorage before navigation', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn().mockResolvedValue({
        ok: true,
        status: 200,
        text: async () =>
          JSON.stringify({
            accountId: 'a',
            orgId: 'o',
            email: 'e@x.com',
            alreadyDone: false,
            apiKey: 'mitos_live_abc123',
            apiKeyId: 'k1',
          }),
      }),
    )
    render(<Verify token="t" />)
    // Wait for the key to appear in the UI - this confirms the fetch resolved.
    await waitFor(() =>
      expect(screen.getByText('mitos_live_abc123')).toBeInTheDocument(),
    )
    // The key must be stashed in sessionStorage so the first-run can read it.
    expect(sessionStorage.getItem('mitos.firstKey')).toBe('mitos_live_abc123')
    vi.unstubAllGlobals()
  })

  it('(c) shows invalid-link message and /signup link on 400 error', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn().mockResolvedValue({
        ok: false,
        status: 400,
        text: async () => '',
      }),
    )
    render(<Verify token="t" />)
    await waitFor(() =>
      expect(screen.getByText(/invalid or has expired/i)).toBeInTheDocument(),
    )
    const signupLink = screen.getByRole('link', { name: /start over/i })
    expect(signupLink).toHaveAttribute('href', '/signup')
    vi.unstubAllGlobals()
  })
})
