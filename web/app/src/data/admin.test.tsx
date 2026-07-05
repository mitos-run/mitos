import { describe, it, expect, vi } from 'vitest'
import { renderHook, waitFor } from '@testing-library/react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { useAdminOverview, useAdminOrgs, useAdminNodes, useAdminWaitlist, useApproveWaitlistEntry, useAdminAudit } from './admin'

function wrapper() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  return ({ children }: { children: React.ReactNode }) => (
    <QueryClientProvider client={client}>{children}</QueryClientProvider>
  )
}

function jsonResponse(body: unknown) {
  return new Response(JSON.stringify(body), { status: 200, headers: { 'content-type': 'application/json' } })
}

describe('useAdminOverview', () => {
  it('fetches the overview document', async () => {
    vi.spyOn(globalThis, 'fetch').mockResolvedValue(
      jsonResponse({
        orgs: 3,
        running_sandboxes: 5,
        running_sandboxes_orgs: 3,
        nodes_ready: 2,
        nodes_total: 2,
        signup_mode: 'waitlist',
      }),
    )
    const { result } = renderHook(() => useAdminOverview(), { wrapper: wrapper() })
    await waitFor(() => expect(result.current.data).toBeDefined())
    expect(result.current.data?.orgs).toBe(3)
    expect(result.current.data?.running_sandboxes_orgs).toBe(3)
    expect(result.current.data?.signup_mode).toBe('waitlist')
  })
})

describe('useAdminOrgs', () => {
  it('fetches the org rollup table', async () => {
    vi.spyOn(globalThis, 'fetch').mockResolvedValue(
      jsonResponse({ orgs: [{ id: 'o1', name: 'Acme', tier: 'free', members: 2, running: 1, month_usage_cents: 500 }], total: 1 }),
    )
    const { result } = renderHook(() => useAdminOrgs(), { wrapper: wrapper() })
    await waitFor(() => expect(result.current.data).toBeDefined())
    expect(result.current.data?.orgs).toHaveLength(1)
    expect(result.current.data?.total).toBe(1)
  })
})

describe('useAdminNodes', () => {
  it('reports available:false honestly when the server has no NodeSource', async () => {
    vi.spyOn(globalThis, 'fetch').mockResolvedValue(jsonResponse({ available: false, nodes: [] }))
    const { result } = renderHook(() => useAdminNodes(), { wrapper: wrapper() })
    await waitFor(() => expect(result.current.data).toBeDefined())
    expect(result.current.data?.available).toBe(false)
    expect(result.current.data?.nodes).toEqual([])
  })
})

describe('useAdminWaitlist / useApproveWaitlistEntry', () => {
  it('fetches entries and invalidates the waitlist query after an approve', async () => {
    const fetchMock = vi.spyOn(globalThis, 'fetch')
    fetchMock.mockResolvedValueOnce(
      jsonResponse({ entries: [{ id: 'ZW1haWxAZXhhbXBsZS5jb20', email: 'email@example.com', created_at: '2026-01-01T00:00:00Z' }] }),
    )
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } })
    const Wrapper = ({ children }: { children: React.ReactNode }) => (
      <QueryClientProvider client={client}>{children}</QueryClientProvider>
    )

    const list = renderHook(() => useAdminWaitlist(), { wrapper: Wrapper })
    await waitFor(() => expect(list.result.current.data).toBeDefined())
    expect(list.result.current.data).toHaveLength(1)

    fetchMock.mockResolvedValueOnce(jsonResponse({ email: 'email@example.com', approved: true, already_approved: false }))
    const approve = renderHook(() => useApproveWaitlistEntry(), { wrapper: Wrapper })
    approve.result.current.mutate('ZW1haWxAZXhhbXBsZS5jb20')
    await waitFor(() => expect(approve.result.current.isSuccess).toBe(true))
    expect(approve.result.current.data?.already_approved).toBe(false)
  })
})

describe('useAdminAudit', () => {
  it('fetches the instance-operator plane audit events', async () => {
    vi.spyOn(globalThis, 'fetch').mockResolvedValue(
      jsonResponse({
        events: [
          {
            org_id: '_instance',
            actor_id: 'a1',
            action: 'admin.overview.view',
            target: '',
            detail: 'viewed the instance operator overview',
            at: '2026-01-01T00:00:00Z',
          },
        ],
      }),
    )
    const { result } = renderHook(() => useAdminAudit(), { wrapper: wrapper() })
    await waitFor(() => expect(result.current.data).toBeDefined())
    expect(result.current.data).toHaveLength(1)
    expect(result.current.data?.[0].action).toBe('admin.overview.view')
  })
})
