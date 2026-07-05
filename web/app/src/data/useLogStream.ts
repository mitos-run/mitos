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
//
// On a deployment that DOES support streaming, the probe's own response is
// itself a live SSE stream: reading only r.status and never closing it would
// leave that connection open forever, leaking one server-side stream and
// goroutine per Live toggle and eating into the browser's per-host
// connection pool. The probe is therefore issued with an AbortController and
// aborted immediately once r.status has been read, on every path (501,
// fall-through to EventSource, and the effect's own cleanup if the
// component unmounts or id/live changes before the probe resolves).
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
    const probeController = new AbortController()

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

    fetch(api.logStreamURL(id), {
      headers: { Accept: 'text/event-stream' },
      signal: probeController.signal,
    })
      .then((r) => {
        // The status has been read; the probe's own connection (a live SSE
        // stream on a deployment that supports it) must not be left open.
        const status = r.status
        probeController.abort()
        if (cancelled) return
        if (status === 501) {
          setUnsupported(true)
          return
        }
        startStreaming()
      })
      .catch((err) => {
        // Our own abort() rejects the fetch promise too; that is expected
        // teardown, not a probe failure, so it must not fall through to
        // opening a second stream or surface as console noise.
        if (err instanceof DOMException && err.name === 'AbortError') return
        // The probe itself failing (e.g. a network error) is a transient
        // condition, not "unsupported": fall back to opening the stream
        // directly so EventSource's own retry logic can take over.
        if (cancelled) return
        startStreaming()
      })

    return () => {
      cancelled = true
      // Abort a still-in-flight probe so an unmounted (or id/live-changed)
      // component can never leave one pending: same leak as never aborting
      // the happy path, just triggered by teardown instead of resolution.
      probeController.abort()
      document.removeEventListener('visibilitychange', onVisibilityChange)
      esRef.current?.close()
      esRef.current = null
    }
  }, [id, live])

  return { lines, connected, unsupported }
}
