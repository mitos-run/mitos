// useLogStream: the Live-logs hook backing SandboxDetail's Logs tab toggle.
// Opens an EventSource against GET /console/sandboxes/{id}/logs/stream while
// `live` is true, appending each SSE "data:" event as one more line. Relies on
// EventSource's OWN native auto-reconnect (it retries on a transient network
// drop unless explicitly closed) rather than reimplementing backoff; the only
// place this hook closes the connection itself is when the tab goes to the
// background (document.hidden), reopening a fresh one when it becomes visible
// again, so a backgrounded tab is not silently burning a connection and
// server-side SSE goroutine forever.
//
// Before ever creating an EventSource, the hook probes the stream URL with a
// plain fetch (Accept: text/event-stream). EventSource cannot tell a hard
// "this deployment does not implement live streaming" (HTTP 501) apart from
// a transient network drop; left alone it just retries forever, which reads
// to the user as a perpetual "reconnecting" state that will never resolve.
// The probe lets a genuine 501 be reported once as `unsupported` with no
// EventSource ever opened (so there is no reconnect loop to speak of),
// while any other status (or a probe failure, treated as transient) falls
// through to opening the stream exactly as before.
import { useEffect, useRef, useState } from 'react'
import { api } from '../api'

export type LogStreamState = {
  lines: string[]
  // connected reflects the EventSource's OWN readyState, not just "we tried":
  // true only while the browser reports the stream open.
  connected: boolean
  // unsupported is true once the pre-flight probe has detected a hard 501
  // from the stream endpoint: this deployment's transport does not
  // implement live log streaming. No EventSource is created and none will
  // be retried while this stays true for the current id/live pair.
  unsupported: boolean
}

export function useLogStream(id: string, live: boolean): LogStreamState {
  const [lines, setLines] = useState<string[]>([])
  const [connected, setConnected] = useState(false)
  const [unsupported, setUnsupported] = useState(false)
  const esRef = useRef<EventSource | null>(null)

  useEffect(() => {
    // Reset the transcript whenever the target sandbox or the live toggle
    // changes, so switching sandboxes never shows a stale tail.
    setLines([])
    setConnected(false)
    setUnsupported(false)
    if (!live || !id) return

    let cancelled = false

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

    function startStreaming() {
      if (!document.hidden) open()
      document.addEventListener('visibilitychange', onVisibilityChange)
    }

    fetch(api.logStreamURL(id), { headers: { Accept: 'text/event-stream' } })
      .then((r) => {
        if (cancelled) return
        if (r.status === 501) {
          setUnsupported(true)
          return
        }
        startStreaming()
      })
      .catch(() => {
        // The probe itself failing (e.g. a network error) is a transient
        // condition, not "unsupported": fall back to opening the stream
        // directly so EventSource's own retry logic can take over.
        if (cancelled) return
        startStreaming()
      })

    return () => {
      cancelled = true
      document.removeEventListener('visibilitychange', onVisibilityChange)
      esRef.current?.close()
      esRef.current = null
    }
  }, [id, live])

  return { lines, connected, unsupported }
}
