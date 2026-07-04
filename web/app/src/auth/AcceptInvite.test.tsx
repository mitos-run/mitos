// AcceptInvite page tests. Mirrors Verify.test.tsx conventions: fetch is
// mocked via vi.stubGlobal so api.ts sees controlled responses without a
// real server.
import { describe, it, expect, vi } from 'vitest'
import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { AcceptInvite } from './AcceptInvite'

function lookupResponse(body: Record<string, unknown>) {
  return { ok: true, status: 200, json: async () => body, text: async () => JSON.stringify(body) }
}

describe('AcceptInvite page (pre-auth)', () => {
  it('shows the org/inviter/email-hint summary and sign-in/create-account CTAs', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn().mockResolvedValue(
        lookupResponse({
          org_name: 'Acme',
          inviter_name: 'Alice',
          email_hint: 'jo***@example.com',
          role: 'admin',
          state: 'pending',
        }),
      ),
    )
    render(<AcceptInvite token="tok-1" authenticated={false} />)
    await waitFor(() => expect(screen.getByText('Acme')).toBeInTheDocument())
    expect(screen.getByText('Alice')).toBeInTheDocument()
    expect(screen.getByText('jo***@example.com')).toBeInTheDocument()

    const signIn = screen.getByRole('link', { name: /sign in to accept/i })
    expect(signIn).toHaveAttribute('href', expect.stringContaining('/login?next='))
    const createAccount = screen.getByRole('link', { name: /create an account/i })
    expect(createAccount).toHaveAttribute('href', '/signup?invite_token=tok-1')
    vi.unstubAllGlobals()
  })

  it('shows an invalid-link message when the token does not resolve', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue({ ok: false, status: 404, json: async () => ({}), text: async () => '' }))
    render(<AcceptInvite token="bogus" authenticated={false} />)
    await waitFor(() => expect(screen.getByText(/invalid or has expired/i)).toBeInTheDocument())
    vi.unstubAllGlobals()
  })

  it('shows an invalid-link message when no token is provided', () => {
    render(<AcceptInvite token="" authenticated={false} />)
    expect(screen.getByText(/invalid or has expired/i)).toBeInTheDocument()
  })
})

describe('AcceptInvite page (post-auth)', () => {
  it('shows a confirm-join button and accepts on click', async () => {
    const fetchMock = vi
      .fn()
      .mockResolvedValueOnce(
        lookupResponse({ org_name: 'Acme', inviter_name: 'Alice', email_hint: 'jo***@example.com', role: 'member', state: 'pending' }),
      )
      .mockResolvedValueOnce({ ok: true, status: 200, text: async () => JSON.stringify({ org_id: 'org-1', role: 'member' }) })
    vi.stubGlobal('fetch', fetchMock)

    render(<AcceptInvite token="tok-2" authenticated />)
    const joinBtn = await screen.findByRole('button', { name: /join acme/i })
    await userEvent.click(joinBtn)

    await waitFor(() => expect(screen.getByRole('link', { name: /continue to console/i })).toBeInTheDocument())
    expect(screen.getByText(/you have joined acme/i)).toBeInTheDocument()
    vi.unstubAllGlobals()
  })

  it('treats an already-accepted invite (e.g. auto-joined at signup) as success, not an error', async () => {
    const fetchMock = vi
      .fn()
      // initial lookup: still shows pending from this viewer's perspective
      .mockResolvedValueOnce(
        lookupResponse({ org_name: 'Acme', inviter_name: 'Alice', email_hint: 'jo***@example.com', role: 'member', state: 'pending' }),
      )
      // accept call fails (already used)
      .mockResolvedValueOnce({ ok: false, status: 400, text: async () => JSON.stringify({ error: { cause: 'already used' } }) })
      // re-lookup reveals it is in fact accepted
      .mockResolvedValueOnce(
        lookupResponse({ org_name: 'Acme', inviter_name: 'Alice', email_hint: 'jo***@example.com', role: 'member', state: 'accepted' }),
      )
    vi.stubGlobal('fetch', fetchMock)

    render(<AcceptInvite token="tok-3" authenticated />)
    const joinBtn = await screen.findByRole('button', { name: /join acme/i })
    await userEvent.click(joinBtn)

    await waitFor(() => expect(screen.getByText(/you have joined acme/i)).toBeInTheDocument())
    expect(screen.queryByText(/could not be accepted/i)).not.toBeInTheDocument()
    vi.unstubAllGlobals()
  })

  it('shows the already-a-member state immediately when the lookup reports accepted', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn().mockResolvedValue(
        lookupResponse({ org_name: 'Acme', inviter_name: 'Alice', email_hint: 'jo***@example.com', role: 'member', state: 'accepted' }),
      ),
    )
    render(<AcceptInvite token="tok-4" authenticated />)
    await waitFor(() => expect(screen.getByText(/you have joined acme/i)).toBeInTheDocument())
    vi.unstubAllGlobals()
  })
})
