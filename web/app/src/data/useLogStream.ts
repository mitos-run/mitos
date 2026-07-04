// useLogStream: the Live-logs hook backing SandboxDetail's Logs tab toggle.
// Opens an EventSource against GET /console/sandboxes/{id}/logs/stream while
// `live` is true, appending each SSE "data:" event as one more line. Relies on
// EventSource's OWN native auto-reconnect (it retries on a transient network
// drop unless explicitly closed) rather than reimplementing backoff; the only
// place this hook closes the connection itself is when the tab goes to the
// background (document.hidden), reopening a fresh one when it becomes visible
// again, so a backgrounded tab is not silently burning a connection and
// server-side SSE goroutine forever.
import { useEffect, useRef, useState } from 'react'
import { api } from '../api'

export type LogStreamState = {
  lines: string[]
  // connected reflects the EventSource's OWN readyState, not just "we tried":
  // true only while the browser reports the stream open.
  connected: boolean
}

export function useLogStream(id: string, live: boolean): LogStreamState {
  const [lines, setLines] = useState<string[]>([])
  const [connected, setConnected] = useState(false)
  const esRef = useRef<EventSource | null>(null)

  useEffect(() => {
    // Reset the transcript whenever the target sandbox or the live toggle
    // changes, so switching sandboxes never shows a stale tail.
    setLines([])
    setConnected(false)
    if (!live || !id) return

    function open() {
      const es = new EventSource(api.logStreamURL(id))
      esRef.current = es
      es.onopen = () => setConnected(true)
      es.onmessage = (ev) => setLines((cur) => [...cur, ev.data])
      // onerror fires on every transient drop too; EventSource retries on its
      // own unless we call close(), so we only reflect the disconnect here.
      es.onerror = () => setConnected(false)
    }

    function onVisibilityChange() {
      if (document.hidden) {
        esRef.current?.close()
        esRef.current = null
        setConnected(false)
      } else if (!esRef.current) {
        open()
      }
    }

    if (!document.hidden) open()
    document.addEventListener('visibilitychange', onVisibilityChange)
    return () => {
      document.removeEventListener('visibilitychange', onVisibilityChange)
      esRef.current?.close()
      esRef.current = null
    }
  }, [id, live])

  return { lines, connected }
}
