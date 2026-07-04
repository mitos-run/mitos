// Behavior tests for useLogStream: opens an EventSource only while `live` is
// true, appends each message as a line, reflects the connection state, closes
// on tab-hidden and reopens a fresh connection on visible-again, and tears
// down cleanly on unmount. Uses a minimal MockEventSource (jsdom has no real
// EventSource/SSE support) that records every instance created so tests can
// assert reconnect behavior.
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { renderHook, act } from '@testing-library/react'
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

beforeEach(() => {
  MockEventSource.instances = []
  vi.stubGlobal('EventSource', MockEventSource)
  setHidden(false)
})

afterEach(() => {
  vi.unstubAllGlobals()
})

describe('useLogStream', () => {
  it('does not open a connection while live is false', () => {
    renderHook(() => useLogStream('sb-1', false))
    expect(MockEventSource.instances.length).toBe(0)
  })

  it('opens against logStreamURL(id) when live is true and appends messages', () => {
    const { result } = renderHook(() => useLogStream('sb-1', true))
    expect(MockEventSource.instances.length).toBe(1)
    expect(MockEventSource.instances[0].url).toBe('/console/sandboxes/sb-1/logs/stream')

    act(() => {
      MockEventSource.instances[0].onopen?.()
    })
    expect(result.current.connected).toBe(true)

    act(() => {
      MockEventSource.instances[0].onmessage?.({ data: 'hello' })
      MockEventSource.instances[0].onmessage?.({ data: 'world' })
    })
    expect(result.current.lines).toEqual(['hello', 'world'])
  })

  it('reflects a transient error as disconnected without closing the stream (native EventSource retry)', () => {
    const { result } = renderHook(() => useLogStream('sb-1', true))
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

  it('closes the connection when the tab is hidden and opens a fresh one when visible again', () => {
    renderHook(() => useLogStream('sb-1', true))
    expect(MockEventSource.instances.length).toBe(1)
    const first = MockEventSource.instances[0]

    act(() => setHidden(true))
    expect(first.closed).toBe(true)

    act(() => setHidden(false))
    expect(MockEventSource.instances.length).toBe(2)
    expect(MockEventSource.instances[1].closed).toBe(false)
  })

  it('closes the connection on unmount', () => {
    const { unmount } = renderHook(() => useLogStream('sb-1', true))
    const es = MockEventSource.instances[0]
    unmount()
    expect(es.closed).toBe(true)
  })

  it('closes and reopens against the new id when the sandbox id changes', () => {
    const { rerender } = renderHook(({ id }) => useLogStream(id, true), { initialProps: { id: 'sb-1' } })
    expect(MockEventSource.instances[0].url).toBe('/console/sandboxes/sb-1/logs/stream')

    rerender({ id: 'sb-2' })
    expect(MockEventSource.instances[0].closed).toBe(true)
    expect(MockEventSource.instances[1].url).toBe('/console/sandboxes/sb-2/logs/stream')
  })
})
