import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { renderHook } from '@testing-library/react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import React from 'react'
import { useFirstActivity } from './firstActivity'
import * as api from '../api'

function wrapper() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  return ({ children }: { children: React.ReactNode }) =>
    React.createElement(QueryClientProvider, { client }, children)
}

describe('useFirstActivity', () => {
  beforeEach(() => {
    vi.useFakeTimers()
  })

  afterEach(() => {
    vi.useRealTimers()
    vi.restoreAllMocks()
  })

  it('polls every 3000 ms while not active, stops once active is true', async () => {
    // First call returns inactive, second call returns active
    const spy = vi
      .spyOn(api, 'firstActivity')
      .mockResolvedValueOnce({ active: false })
      .mockResolvedValueOnce({ active: true })

    const { result } = renderHook(() => useFirstActivity(true), { wrapper: wrapper() })

    // Let initial fetch complete - flush timers and pending promises
    await vi.advanceTimersByTimeAsync(100)

    expect(spy).toHaveBeenCalledTimes(1)
    expect(result.current.data?.active).toBe(false)

    // Advance past the 3000 ms refetch interval to trigger second poll
    await vi.advanceTimersByTimeAsync(3100)

    expect(spy).toHaveBeenCalledTimes(2)
    expect(result.current.data?.active).toBe(true)

    // Advance again - refetchInterval should return false once active, so no third call
    await vi.advanceTimersByTimeAsync(3100)

    expect(spy).toHaveBeenCalledTimes(2)
  })

  it('does not fetch when disabled', async () => {
    const spy = vi.spyOn(api, 'firstActivity').mockResolvedValue({ active: false })

    renderHook(() => useFirstActivity(false), { wrapper: wrapper() })

    // Advance time - no fetch should happen since enabled is false
    await vi.advanceTimersByTimeAsync(3100)

    expect(spy).not.toHaveBeenCalled()
  })
})
