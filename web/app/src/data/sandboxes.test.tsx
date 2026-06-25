import { describe, it, expect, vi } from 'vitest'
import { renderHook, waitFor, act } from '@testing-library/react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { useSandboxes, useTerminateSandbox } from './sandboxes'

function harness() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } })
  const wrapper = ({ children }: { children: React.ReactNode }) => (
    <QueryClientProvider client={client}>{children}</QueryClientProvider>
  )
  return { client, wrapper }
}

describe('sandboxes data layer', () => {
  it('lists sandboxes', async () => {
    vi.spyOn(globalThis, 'fetch').mockResolvedValue(
      new Response(JSON.stringify({ sandboxes: [{ id: 's1', org_id: 'o', template: 't', node: 'n', phase: 'Running', vcpus: 2, mem_bytes: 1024, created_at: '2026-01-01T00:00:00Z' }] }),
        { status: 200, headers: { 'content-type': 'application/json' } }))
    const { wrapper } = harness()
    const { result } = renderHook(() => useSandboxes(), { wrapper })
    await waitFor(() => expect(result.current.data).toBeDefined())
    expect(result.current.data?.[0].id).toBe('s1')
  })

  it('optimistically removes a terminated sandbox from the list cache', async () => {
    const { client, wrapper } = harness()
    client.setQueryData(['sandboxes'], [{ id: 's1', org_id: 'o', template: 't', node: 'n', phase: 'Running', vcpus: 1, mem_bytes: 1, created_at: '' }, { id: 's2', org_id: 'o', template: 't', node: 'n', phase: 'Running', vcpus: 1, mem_bytes: 1, created_at: '' }])
    vi.spyOn(globalThis, 'fetch').mockResolvedValue(new Response(null, { status: 200 }))
    const { result } = renderHook(() => useTerminateSandbox(), { wrapper })
    await act(async () => { await result.current.mutateAsync('s1') })
    const list = client.getQueryData(['sandboxes']) as Array<{ id: string }>
    expect(list.find((s) => s.id === 's1')).toBeUndefined()
    expect(list.find((s) => s.id === 's2')).toBeDefined()
  })
})
