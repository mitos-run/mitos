import { describe, it, expect, vi } from 'vitest'
import { renderHook, waitFor } from '@testing-library/react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { useInstruments } from './instruments'

function wrapper() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  return ({ children }: { children: React.ReactNode }) => (
    <QueryClientProvider client={client}>{children}</QueryClientProvider>
  )
}

describe('useInstruments', () => {
  it('fetches the org instruments document', async () => {
    vi.spyOn(globalThis, 'fetch').mockResolvedValue(
      new Response(JSON.stringify({ org_id: 'o1', activate_p50_ms: 27, activate_p99_ms: 41, forks_served: 10, cow_savings_bytes: 2304, marginal_bytes_per_fork: 3 }), {
        status: 200, headers: { 'content-type': 'application/json' },
      }),
    )
    const { result } = renderHook(() => useInstruments(), { wrapper: wrapper() })
    await waitFor(() => expect(result.current.data).toBeDefined())
    expect(result.current.data?.activate_p50_ms).toBe(27)
  })
})
