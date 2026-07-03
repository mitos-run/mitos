// The console root: mounts the provider stack (query cache, toasts) and the
// capability-built router. The shell, nav, and routing all live below; this
// file only assembles providers. Capabilities are fetched once here so the
// router is built from a known edition before first paint of the shell.
import { QueryClientProvider } from '@tanstack/react-query'
import { RouterProvider } from '@tanstack/react-router'
import { useMemo } from 'react'
import { UnauthorizedError } from './api'
import { queryClient, useCapabilities } from './data/query'
import { createPreAuthRouter } from './auth/preauthRouter'
import { createConsoleRouter } from './router'
import { LoadingScreen } from './ui/LoadingScreen'
import { ToastProvider } from './ui/Toast'

function RoutedConsole() {
  const { data: caps, error } = useCapabilities()
  // Memoize both routers so they are not recreated on every render.
  const preAuthRouter = useMemo(() => createPreAuthRouter(), [])
  const consoleRouter = useMemo(() => (caps ? createConsoleRouter(caps) : null), [caps])
  if (error instanceof UnauthorizedError) return <RouterProvider router={preAuthRouter} />
  if (error) return <main style={{ padding: 32 }}><div className="t-dim">console unavailable: {String(error)}</div></main>
  if (!caps || !consoleRouter) return <LoadingScreen />
  return <RouterProvider router={consoleRouter} />
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
