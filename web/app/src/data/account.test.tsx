import { describe, it, expect, vi } from 'vitest'
import { renderHook, waitFor } from '@testing-library/react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { useKeys } from './account'

function wrapper() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  return ({ children }: { children: React.ReactNode }) => <QueryClientProvider client={client}>{children}</QueryClientProvider>
}

describe('useKeys', () => {
  it('lists api keys (masked)', async () => {
    vi.spyOn(globalThis, 'fetch').mockResolvedValue(new Response(JSON.stringify({ org_id: 'o', keys: [{ id: 'k1', name: 'ci', prefix: 'mitos_live_ab12', scopes: ['sandboxes'], created_at: '', revoked: false }] }), { status: 200, headers: { 'content-type': 'application/json' } }))
    const { result } = renderHook(() => useKeys(), { wrapper: wrapper() })
    await waitFor(() => expect(result.current.data).toBeDefined())
    expect(result.current.data?.[0].prefix).toBe('mitos_live_ab12')
  })
})
