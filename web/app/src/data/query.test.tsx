import { describe, it, expect, vi, beforeEach } from 'vitest'
import { renderHook, waitFor } from '@testing-library/react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { useCapabilities } from './query'

function wrapper() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  return ({ children }: { children: React.ReactNode }) => (
    <QueryClientProvider client={client}>{children}</QueryClientProvider>
  )
}

describe('useCapabilities', () => {
  beforeEach(() => {
    vi.restoreAllMocks()
  })

  it('fetches the capabilities document', async () => {
    vi.spyOn(globalThis, 'fetch').mockResolvedValue(
      new Response(JSON.stringify({ edition: 'community', billing: false, teams: true }), {
        status: 200,
        headers: { 'content-type': 'application/json' },
      }),
    )
    const { result } = renderHook(() => useCapabilities(), { wrapper: wrapper() })
    await waitFor(() => expect(result.current.data).toBeDefined())
    expect(result.current.data?.edition).toBe('community')
  })
})
