// Behavior tests for useLogStream: opens an EventSource only while `live` is
// true, appends each message as a line, reflects the connection state, closes
// on tab-hidden and reopens a fresh connection on visible-again, and tears
// down cleanly on unmount. Uses a minimal MockEventSource (jsdom has no real
// EventSource/SSE support) that records every instance created so tests can
// assert reconnect behavior.
//
// Before opening an EventSource, the hook probes the stream URL with fetch:
// a hard 501 (this deployment's transport does not implement live streaming)
// is detected up front and reported as `unsupported`, WITHOUT ever opening
// an EventSource (which would otherwise retry forever and read as a
// perpetual "reconnecting" state to the user). Every test below mocks fetch
// so the probe resolves deterministically; a 200 default lets the existing
// EventSource-opening behavior proceed unchanged.
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { renderHook, act, waitFor } from '@testing-library/react'
import { useLogStream } from './useLogStream'

class MockEventSource {
  static instances: MockEventSource[] = []
  url: string
  onopen: (() => void) | null = null
  onmessage: ((ev: { data: string }) => void) | null = null
  onerror: (() => void) | null = null
  closed = false

  constructor(url: string) {
    this.url = url
    MockEventSource.instances.push(this)
  }

  close() {
    this.closed = true
  }
}

function setHidden(hidden: boolean) {
  Object.defineProperty(document, 'hidden', { configurable: true, get: () => hidden })
  document.dispatchEvent(new Event('visibilitychange'))
}

function mockProbeStatus(status: number) {
  vi.spyOn(globalThis, 'fetch').mockImplementation(() => Promise.resolve(new Response(null, { status })))
}

// Captures the AbortSignal passed to the probe fetch so a test can assert it
// was aborted once the probe resolves, instead of just its resolved status.
function mockProbeStatusCapturingSignal(status: number) {
  const init: { signal?: AbortSignal }[] = []
  vi.spyOn(globalThis, 'fetch').mockImplementation((_url, opts) => {
    init.push((opts ?? {}) as { signal?: AbortSignal })
    return Promise.resolve(new Response(null, { status }))
  })
  return init
}

beforeEach(() => {
  MockEventSource.instances = []
  vi.stubGlobal('EventSource', MockEventSource)
  setHidden(false)
  mockProbeStatus(200)
})

afterEach(() => {
  vi.unstubAllGlobals()
  vi.restoreAllMocks()
})

describe('useLogStream', () => {
  it('does not open a connection while live is false', () => {
    renderHook(() => useLogStream('sb-1', false))
    expect(MockEventSource.instances.length).toBe(0)
  })

  it('opens against logStreamURL(id) when live is true and appends messages', async () => {
    const { result } = renderHook(() => useLogStream('sb-1', true))
    await waitFor(() => expect(MockEventSource.instances.length).toBe(1))
    expect(MockEventSource.instances[0].url).toBe('/console/sandboxes/sb-1/logs/stream')

    act(() => {
      MockEventSource.instances[0].onopen?.()
    })
    expect(result.current.connected).toBe(true)
    expect(result.current.unsupported).toBe(false)

    act(() => {
      MockEventSource.instances[0].onmessage?.({ data: 'hello' })
      MockEventSource.instances[0].onmessage?.({ data: 'world' })
    })
    expect(result.current.lines).toEqual(['hello', 'world'])
  })

  it('reflects a transient error as disconnected without closing the stream (native EventSource retry)', async () => {
    const { result } = renderHook(() => useLogStream('sb-1', true))
    await waitFor(() => expect(MockEventSource.instances.length).toBe(1))
    act(() => {
      MockEventSource.instances[0].onopen?.()
    })
    expect(result.current.connected).toBe(true)

    act(() => {
      MockEventSource.instances[0].onerror?.()
    })
    expect(result.current.connected).toBe(false)
    // The hook itself never calls close() on a transient error: that is left
    // to the browser's own EventSource retry logic.
    expect(MockEventSource.instances[0].closed).toBe(false)
  })

  it('closes the connection when the tab is hidden and opens a fresh one when visible again', async () => {
    renderHook(() => useLogStream('sb-1', true))
    await waitFor(() => expect(MockEventSource.instances.length).toBe(1))
    const first = MockEventSource.instances[0]

    act(() => setHidden(true))
    expect(first.closed).toBe(true)

    act(() => setHidden(false))
    await waitFor(() => expect(MockEventSource.instances.length).toBe(2))
    expect(MockEventSource.instances[1].closed).toBe(false)
  })

  it('closes the connection on unmount', async () => {
    const { unmount } = renderHook(() => useLogStream('sb-1', true))
    await waitFor(() => expect(MockEventSource.instances.length).toBe(1))
    const es = MockEventSource.instances[0]
    unmount()
    expect(es.closed).toBe(true)
  })

  it('closes and reopens against the new id when the sandbox id changes', async () => {
    const { rerender } = renderHook(({ id }) => useLogStream(id, true), { initialProps: { id: 'sb-1' } })
    await waitFor(() => expect(MockEventSource.instances.length).toBe(1))
    expect(MockEventSource.instances[0].url).toBe('/console/sandboxes/sb-1/logs/stream')

    rerender({ id: 'sb-2' })
    expect(MockEventSource.instances[0].closed).toBe(true)
    await waitFor(() => expect(MockEventSource.instances.length).toBe(2))
    expect(MockEventSource.instances[1].url).toBe('/console/sandboxes/sb-2/logs/stream')
  })

  it('aborts the probe fetch once it resolves and streaming starts (no leaked stream)', async () => {
    const calls = mockProbeStatusCapturingSignal(200)
    renderHook(() => useLogStream('sb-1', true))
    await waitFor(() => expect(MockEventSource.instances.length).toBe(1))

    expect(calls.length).toBe(1)
    expect(calls[0].signal).toBeInstanceOf(AbortSignal)
    // The probe is a live SSE connection on a deployment that supports
    // streaming: if it is never aborted, it leaks one server-side stream and
    // goroutine per Live toggle and eats the browser's per-host connection
    // pool. Once the probe has resolved and the real EventSource has been
    // opened, the probe request itself must be torn down.
    expect(calls[0].signal?.aborted).toBe(true)
  })

  // --- 501 (unsupported transport) ---

  it('never opens an EventSource and reports unsupported when the stream probe returns 501', async () => {
    mockProbeStatus(501)
    const { result } = renderHook(() => useLogStream('sb-1', true))
    await waitFor(() => expect(result.current.unsupported).toBe(true))
    expect(MockEventSource.instances.length).toBe(0)
    expect(result.current.connected).toBe(false)
  })

  it('does not loop retrying the probe once unsupported is set (no reconnect loop)', async () => {
    mockProbeStatus(501)
    const { result, rerender } = renderHook(() => useLogStream('sb-1', true))
    await waitFor(() => expect(result.current.unsupported).toBe(true))
    const fetchCallsAfterFirstProbe = (globalThis.fetch as ReturnType<typeof vi.fn>).mock.calls.length

    // Re-rendering with the same id/live must not re-probe or open an
    // EventSource: the effect only reruns when id or live changes.
    rerender()
    expect((globalThis.fetch as ReturnType<typeof vi.fn>).mock.calls.length).toBe(fetchCallsAfterFirstProbe)
    expect(MockEventSource.instances.length).toBe(0)
  })

  it('recovers from unsupported when the sandbox id changes and the new probe succeeds', async () => {
    mockProbeStatus(501)
    const { result, rerender } = renderHook(({ id }) => useLogStream(id, true), { initialProps: { id: 'sb-1' } })
    await waitFor(() => expect(result.current.unsupported).toBe(true))

    mockProbeStatus(200)
    rerender({ id: 'sb-2' })
    await waitFor(() => expect(MockEventSource.instances.length).toBe(1))
    expect(result.current.unsupported).toBe(false)
  })
})
