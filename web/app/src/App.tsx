// The console root: mounts the provider stack (query cache, toasts) and the
// capability-built router. The shell, nav, and routing all live below; this
// file only assembles providers. Capabilities are fetched once here so the
// router is built from a known edition before first paint of the shell.
import { QueryClientProvider } from '@tanstack/react-query'
import { RouterProvider } from '@tanstack/react-router'
import { useMemo } from 'react'
import { queryClient, useCapabilities } from './data/query'
import { createConsoleRouter } from './router'
import { ToastProvider } from './ui/Toast'

function RoutedConsole() {
  const { data: caps, error } = useCapabilities()
  const router = useMemo(() => (caps ? createConsoleRouter(caps) : null), [caps])
  if (error) return <main style={{ padding: 32 }}><div className="t-dim">console unavailable: {String(error)}</div></main>
  if (!caps || !router) return <main style={{ padding: 32 }}><div className="t-dim">loading...</div></main>
  return <RouterProvider router={router} />
}

export function App() {
  return (
    <QueryClientProvider client={queryClient}>
      <ToastProvider>
        <div className="field" />
        <RoutedConsole />
      </ToastProvider>
    </QueryClientProvider>
  )
}
