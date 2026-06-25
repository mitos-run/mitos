import { describe, it, expect, vi } from 'vitest'
import { renderHook, waitFor } from '@testing-library/react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { useForkTree } from './forktree'

function wrapper() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  return ({ children }: { children: React.ReactNode }) => (
    <QueryClientProvider client={client}>{children}</QueryClientProvider>
  )
}

describe('useForkTree', () => {
  it('fetches the org fork tree', async () => {
    vi.spyOn(globalThis, 'fetch').mockResolvedValue(
      new Response(JSON.stringify({ org_id: 'o1', nodes: [{ id: 's1', parent_id: '', phase: 'Running', private_dirty_bytes: 3, shared_bytes: 200 }] }), {
        status: 200, headers: { 'content-type': 'application/json' },
      }),
    )
    const { result } = renderHook(() => useForkTree(), { wrapper: wrapper() })
    await waitFor(() => expect(result.current.data).toBeDefined())
    expect(result.current.data?.nodes[0].id).toBe('s1')
  })
})
