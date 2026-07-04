import { describe, it, expect, vi } from 'vitest'
import { renderHook, waitFor, act } from '@testing-library/react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { useSandboxes, useTerminateSandbox, useCreateSandbox, useForkSandbox, useExecSandbox } from './sandboxes'

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

  it('useCreateSandbox inserts the created sandbox into the list cache', async () => {
    const { client, wrapper } = harness()
    client.setQueryData(['sandboxes'], [{ id: 's1', org_id: 'o', template: 't', node: 'n', phase: 'Running', vcpus: 1, mem_bytes: 1, created_at: '' }])
    const created = { id: 's2', org_id: 'o', template: 'py', node: '', phase: 'Pending', vcpus: 1, mem_bytes: 1 << 30, created_at: '' }
    vi.spyOn(globalThis, 'fetch').mockResolvedValue(
      new Response(JSON.stringify(created), { status: 201, headers: { 'content-type': 'application/json' } }),
    )
    const { result } = renderHook(() => useCreateSandbox(), { wrapper })
    await act(async () => {
      await result.current.mutateAsync({ template: 'py', vcpus: 1, mem_gib: 1 })
    })
    const list = client.getQueryData(['sandboxes']) as Array<{ id: string }>
    expect(list.map((s) => s.id)).toEqual(['s1', 's2'])
  })

  it('useForkSandbox posts count and returns the new ids', async () => {
    const { wrapper } = harness()
    vi.spyOn(globalThis, 'fetch').mockResolvedValue(
      new Response(JSON.stringify({ org_id: 'o', source: 's1', ids: ['s1-fork-1', 's1-fork-2'] }), {
        status: 200,
        headers: { 'content-type': 'application/json' },
      }),
    )
    const { result } = renderHook(() => useForkSandbox(), { wrapper })
    let res: { ids: string[] } | null = null
    await act(async () => {
      res = await result.current.mutateAsync({ id: 's1', count: 2 })
    })
    expect(res).toEqual({ org_id: 'o', source: 's1', ids: ['s1-fork-1', 's1-fork-2'] })
  })

  it('useExecSandbox posts cmd/timeout_s and returns the result', async () => {
    const { wrapper } = harness()
    let sentBody: unknown = null
    vi.spyOn(globalThis, 'fetch').mockImplementation((_input, init) => {
      sentBody = init?.body ? JSON.parse(String(init.body)) : null
      return Promise.resolve(
        new Response(JSON.stringify({ stdout: 'ok\n', stderr: '', exit_code: 0 }), {
          status: 200,
          headers: { 'content-type': 'application/json' },
        }),
      )
    })
    const { result } = renderHook(() => useExecSandbox(), { wrapper })
    let res: { stdout: string; stderr: string; exit_code: number } | null = null
    await act(async () => {
      res = await result.current.mutateAsync({ id: 's1', cmd: 'echo hi', timeoutS: 5 })
    })
    expect(sentBody).toEqual({ cmd: 'echo hi', timeout_s: 5 })
    expect(res).toEqual({ stdout: 'ok\n', stderr: '', exit_code: 0 })
  })
})
